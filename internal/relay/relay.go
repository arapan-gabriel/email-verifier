package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// The collaborators this package needs, declared here in the package that uses
// them (ENGINEERING-STANDARDS §2). They are the same ones verification uses,
// and that is the point: the receiving server sees one IP.
type (
	// Resolver returns the MX hosts for a domain, already vetted against the
	// SSRF guard (invariant 2). A recipient domain chooses its own MX, so it is
	// attacker-influenced here exactly as it is for verification.
	Resolver interface {
		MX(ctx context.Context, domain string) ([]string, error)
		Resolve(ctx context.Context, host string) ([]netip.Addr, error)
	}
	// Dialer opens the connection. Injected so a test never needs a socket.
	Dialer interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
	// Pacer is the same one verification uses, deliberately: a busy MX slows
	// both legs, because the server on the other end sees one address.
	Pacer interface {
		Acquire(ctx context.Context, mxHost, domain string) error
		Observe(ctx context.Context, mxHost string, throttled bool)
	}
	// Suppression is checked before every send and **fails closed** here, which
	// is the difference from the verify path. Verification has an authoritative
	// check upstream; between this queue and the socket there is nothing.
	Suppression interface {
		Suppressed(ctx context.Context, email string) (bool, string, error)
		Enforcing() bool
		Stale(ctx context.Context) bool
	}
	// Health reports whether the sending IP is listed. A listing stops sending
	// and leaves verification alone — see DeliverNext.
	Health interface {
		Burned() bool
	}
	// Recorder counts what happened. Nil means nobody is counting.
	Recorder interface {
		Sent(outcome string)
		QueueDepth(ready, later, dead int)
	}
)

// Options configures a Relay.
type Options struct {
	Queue    *Queue
	Signer   *Signer
	Resolver Resolver
	Dialer   Dialer
	Pacer    Pacer
	Suppress Suppression
	Health   Health
	Metrics  Recorder

	Helo string
	// ReturnPath builds the envelope sender for a message id — VERP, so a
	// bounce identifies its message without the body being parsed.
	ReturnPath func(id string) string
	Timeout    time.Duration
	RetryBase  time.Duration
	// Network is "tcp4". Invariant 3 applies to sending exactly as it does to
	// probing: the published identity covers the IPv4 address alone.
	Network string
}

// Relay drains the queue.
type Relay struct {
	opts Options
	now  func() time.Time
}

// New returns a Relay with the defaults filled in.
func New(opts Options) *Relay {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = 5 * time.Minute
	}
	if opts.Network == "" {
		opts.Network = "tcp4"
	}
	return &Relay{opts: opts, now: time.Now}
}

// ErrNotSending is returned when the node must not send at all right now. It is
// not a failure of any one message, and the queue is left untouched.
var ErrNotSending = errors.New("relay: not sending")

// Accept validates, assembles, signs and queues one message. It returns once
// the message is durable — not once it is delivered.
func (r *Relay) Accept(ctx context.Context, m Message) (string, error) {
	id, err := randomToken(18)
	if err != nil {
		return "", err
	}
	messageID := id + "@" + r.opts.Signer.Domain

	// Refuse the forgotten before anything else, exactly as the prober does
	// (invariant 9) — and here a list we cannot read is fatal, because sending
	// cannot be taken back.
	if err := r.suppressionAllows(ctx, m.To); err != nil {
		return "", err
	}

	headers, body, err := m.Build(r.now(), messageID)
	if err != nil {
		return "", err
	}
	signature, err := r.opts.Signer.Sign(headers, body)
	if err != nil {
		return "", err
	}
	data := append([]byte(signature), Render(headers, body)...)

	item := Item{
		ID:       id,
		MailFrom: r.opts.ReturnPath(id),
		RcptTo:   addressOnly(m.To),
		Data:     data,
		Queued:   r.now(),
	}
	if err := r.opts.Queue.Enqueue(ctx, item); err != nil {
		return "", err
	}
	return messageID, nil
}

// DeliverNext takes one message and tries to send it. Reports whether there was
// anything to do.
func (r *Relay) DeliverNext(ctx context.Context) (bool, error) {
	// A listed IP stops sending entirely. Verification continues — it asks
	// questions rather than delivering, and stopping it would not improve our
	// standing — but every message sent from a listed address makes the listing
	// harder to shake.
	if r.opts.Health != nil && r.opts.Health.Burned() {
		return false, ErrNotSending
	}

	item, ok, err := r.opts.Queue.Next(ctx)
	if err != nil || !ok {
		return false, err
	}
	if r.opts.Metrics != nil {
		ready, later, dead := r.opts.Queue.Depth(ctx)
		r.opts.Metrics.QueueDepth(ready, later, dead)
	}

	// Re-checked at send time, not only at accept time: a message can sit in
	// this queue for hours, and an erasure request that arrived meanwhile must
	// win.
	if err := r.suppressionAllows(ctx, item.RcptTo); err != nil {
		r.record("suppressed")
		return true, r.opts.Queue.Done(ctx, item.ID)
	}

	domain := domainOf(item.RcptTo)
	hosts, err := r.opts.Resolver.MX(ctx, domain)
	if err != nil || len(hosts) == 0 {
		r.record("no_mx")
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase, "no usable MX for "+domain)
	}
	mxHost := hosts[0]

	// One pacing model for the IP. A busy MX slows sending and verification
	// alike, because the server on the other end sees one address.
	if err := r.opts.Pacer.Acquire(ctx, mxHost, domain); err != nil {
		r.record("no_budget")
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase, "no budget: "+err.Error())
	}

	addrs, err := r.opts.Resolver.Resolve(ctx, mxHost)
	if err != nil || len(addrs) == 0 {
		r.record("guarded")
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase, "no vetted address for "+mxHost)
	}

	conn, err := r.opts.Dialer.DialContext(ctx,
		r.opts.Network, net.JoinHostPort(addrs[0].String(), "25"))
	if err != nil {
		r.opts.Pacer.Observe(ctx, mxHost, true)
		r.record("dial_failed")
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase, err.Error())
	}

	out := Send(ctx, conn, SendOptions{
		MXHost: mxHost, Helo: r.opts.Helo,
		MailFrom: item.MailFrom, RcptTo: item.RcptTo,
		Data: item.Data, Timeout: r.opts.Timeout,
	})

	switch {
	case out.Delivered:
		r.opts.Pacer.Observe(ctx, mxHost, false)
		r.record("delivered")
		return true, r.opts.Queue.Done(ctx, item.ID)
	case out.Permanent:
		// A 5xx is an answer. Retrying spends the IP's standing on a question
		// already settled, and the address belongs in the caller's records —
		// which plan 015 will do from the bounce rather than from here.
		r.opts.Pacer.Observe(ctx, mxHost, false)
		r.record("rejected")
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase,
			fmt.Sprintf("%d %s", out.Code, firstLine(out.Reply)))
	default:
		r.opts.Pacer.Observe(ctx, mxHost, true)
		r.record("deferred")
		reason := out.Err
		if reason == "" {
			reason = fmt.Sprintf("%d %s", out.Code, firstLine(out.Reply))
		}
		return true, r.opts.Queue.Retry(ctx, item, r.opts.RetryBase, reason)
	}
}

// suppressionAllows fails closed. A stale list is a refusal here: on the verify
// path it is loud but survivable because the authoritative check ran upstream,
// and sending has nothing between the queue and the socket.
func (r *Relay) suppressionAllows(ctx context.Context, addr string) error {
	s := r.opts.Suppress
	if s == nil || !s.Enforcing() {
		return nil
	}
	if s.Stale(ctx) {
		return fmt.Errorf("%w: the suppression list is stale and sending cannot be taken back", ErrNotSending)
	}
	hit, reason, err := s.Suppressed(ctx, addr)
	if err != nil {
		return fmt.Errorf("%w: the suppression list is unreadable: %w", ErrNotSending, err)
	}
	if hit {
		return fmt.Errorf("relay: address is suppressed: %s", reason)
	}
	return nil
}

func (r *Relay) record(outcome string) {
	if r.opts.Metrics != nil {
		r.opts.Metrics.Sent(outcome)
	}
}

func addressOnly(s string) string {
	if start := strings.LastIndex(s, "<"); start != -1 {
		if end := strings.Index(s[start:], ">"); end != -1 {
			return s[start+1 : start+end]
		}
	}
	return strings.TrimSpace(s)
}

func domainOf(addr string) string {
	if at := strings.LastIndex(addr, "@"); at != -1 {
		return strings.ToLower(addr[at+1:])
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i != -1 {
		return s[:i]
	}
	return s
}
