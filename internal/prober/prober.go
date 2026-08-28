package prober

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
)

// Resolver turns the caller-supplied MX host into addresses that are safe to
// connect to.
//
// Declared here so the prober can never be handed a hostname to resolve for
// itself: the guard has to sit between the lookup and the socket, and passing
// a name to a Dialer would put it on the wrong side (invariant 2).
type Resolver interface {
	Resolve(ctx context.Context, host string) ([]netip.Addr, error)
}

// Health reports whether this node's sending IP is still usable.
//
// Consulted before anything else: if the IP is listed somewhere that matters,
// every probe deepens the damage and none of them produce answers worth having.
type Health interface {
	Burned() (bool, string)
	ObservePolicy(mxHost string)
}

// Suppression refuses addresses somebody asked to be forgotten.
//
// The error return is separate from the verdict on purpose: the caller decides
// what an unreadable list means, and on the verify path it means "carry on",
// because Data Scout has already checked the authoritative copy.
type Suppression interface {
	Suppressed(ctx context.Context, email string) (bool, string, error)
	Enabled() bool
}

// Recorder counts what happened. Nil means nobody is counting; the prober
// behaves identically either way.
type Recorder interface {
	Result(class string)
	Reply(code int, class string)
	Blocked(reason string)
}

// Pacer holds this MX to a rate it tolerates.
//
// The Observe signature is deliberately a bare bool. Only a genuine rate signal
// may move the pacer (invariant 6), and passing Class.IsThrottle() rather than
// the Class itself means a deferral or a policy block cannot reach it even by
// mistake: greylisting is rate-independent, and slowing down does not grow a
// PTR record.
type Pacer interface {
	Acquire(ctx context.Context, mxHost, domain string) error
	Observe(ctx context.Context, mxHost string, throttled bool)
}

// Profiles remembers per-server facts across requests.
//
// A store failure must degrade to "probe again", never to a failed request:
// unlike the rate budget, not knowing whether a host randomises costs accuracy,
// not safety.
type Profiles interface {
	IsRandomiser(ctx context.Context, mxHost string) bool
	MarkRandomiser(ctx context.Context, mxHost string)
}

// Dialer opens the connection to the recipient MX.
//
// Declared here, in the package that uses it, and holding the one method the
// prober needs (ENGINEERING-STANDARDS §2). net.Dialer satisfies it; a test
// satisfies it with a net.Pipe and never opens a socket.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options configures a Prober. Zero values fall back to safe defaults.
type Options struct {
	Helo     string
	MailFrom string
	// Timeout bounds the whole session, not one command.
	Timeout time.Duration
	// DialNetwork must be "tcp4" (invariant 3). A bare "tcp" on a dual-stack
	// host prefers IPv6 and leaves from an address with no FCrDNS and no SPF,
	// which providers answer with a 5.7.x that this service reads as
	// ClassPolicy — every result would silently become unusable.
	DialNetwork string
	Port        string
	// MaxRCPTPerSession splits a batch. An unbounded recipient list is itself
	// a harvesting signal, and servers commonly cap it near 100.
	MaxRCPTPerSession int
	Dialer            Dialer
	// Resolver defaults to a guarded one. It is a default rather than a
	// required field on purpose: forgetting to supply it must not be a way to
	// end up with an unguarded prober.
	Resolver Resolver
	// Pacer bounds the rate to each MX. Nil means unpaced, which is only ever
	// acceptable in a unit test driving a fake server.
	Pacer Pacer
	// Profiles remembers randomiser verdicts. Nil means every request
	// rediscovers them.
	Profiles Profiles
	// Metrics counts results, replies and refusals. Optional.
	Metrics Recorder
	// Health stands the node down when its IP is burned. Optional.
	Health Health
	// Suppress refuses addresses that must never be contacted. Optional.
	Suppress Suppression
	// OnSuppressionError is called when the list cannot be read. Nil discards
	// it — but something should be listening, because a silent redundancy is
	// no redundancy.
	OnSuppressionError func(error)
	// DeferralRetry is the retry hint given when the server offers none.
	DeferralRetry time.Duration
	// PolicyStop is how many *consecutive* ClassPolicy replies end a session.
	// Zero disables it. A server that refuses the client refuses it for the
	// whole session, so every remaining RCPT spends a token on a question whose
	// answer is already known — and keeps hammering a server that has just told
	// us to go away.
	PolicyStop int
	// CatchAllProbes is how many known-bad local parts to try. One is enough to
	// catch a plain catch-all but not a host that answers by coin flip, where a
	// single probe reports catch-all on one run and clean on the next.
	CatchAllProbes int
}

func (o Options) helo() string {
	if o.Helo != "" {
		return o.Helo
	}
	return "localhost"
}

func (o Options) mailFrom() string {
	if o.MailFrom != "" {
		return o.MailFrom
	}
	// An empty envelope sender is the polite choice, but many MXes treat <>
	// plus RCPT as a bounce probe, so use a real-looking address.
	return "verify@localhost"
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 20 * time.Second
}

func (o Options) network() string {
	if o.DialNetwork != "" {
		return o.DialNetwork
	}
	return "tcp4"
}

func (o Options) port() string {
	if o.Port != "" {
		return o.Port
	}
	return "25"
}

// deferralRetry is how long to tell the caller to wait when the server gives no
// hint of its own. Most greylisters open after five to fifteen minutes.
func (o Options) deferralRetry() time.Duration {
	if o.DeferralRetry > 0 {
		return o.DeferralRetry
	}
	return 15 * time.Minute
}

func (o Options) policyStop() int { return o.PolicyStop }

func (o Options) catchAllProbes() int {
	if o.CatchAllProbes > 0 {
		return o.CatchAllProbes
	}
	return 3
}

func (o Options) maxRCPT() int {
	if o.MaxRCPTPerSession > 0 {
		return o.MaxRCPTPerSession
	}
	return 50
}

func (o Options) dialer() Dialer {
	if o.Dialer != nil {
		return o.Dialer
	}
	return &net.Dialer{Timeout: o.timeout()}
}

func (o Options) resolveVia() Resolver {
	if o.Resolver != nil {
		return o.Resolver
	}
	return resolver.New(resolver.Options{})
}

// Request is one batch scoped to one recipient MX (ADR-006). The caller has
// already run the cheap local layers, resolved the MX and grouped these
// addresses by domain.
type Request struct {
	MXHost       string
	Domain       string
	Emails       []string
	NeedCatchAll bool
	Helo         string
	MailFrom     string
}

// Result is what the session established about one address.
//
// Connected, Accepted and CatchAll are tri-state: nil means the server never
// gave a usable answer, which is a different fact from false. They map
// one-to-one onto Data Scout's existing ProbeResult.
type Result struct {
	Connected *bool `json:"connected"`
	Accepted  *bool `json:"accepted"`
	CatchAll  *bool `json:"catch_all"`
	// Randomiser is a property of the *server*, not of this domain: the host
	// answers inconsistently, so no 250 from it is trustworthy anywhere it is
	// the MX. A randomiser also sets CatchAll, which is the conservative
	// reading and the field existing callers already handle correctly.
	Randomiser   *bool  `json:"randomiser"`
	SMTPCode     int    `json:"smtp_code,omitempty"`
	EnhancedCode string `json:"enhanced_code,omitempty"`
	Class        Class  `json:"class"`
	Reply        string `json:"reply,omitempty"`
	Err          string `json:"err,omitempty"`
	// RetryAfterSeconds is set only when the class means "come back later". It
	// is exact for a paused MX and a hint otherwise. Blind backoff retries a
	// greylisted address seconds later, when the window has not opened, and
	// burns a token to be told the same thing.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// Response carries one Result per requested address.
type Response struct {
	Results map[string]Result `json:"results"`
}

// teardownTimeout bounds the best-effort RSET/QUIT written after the answers
// are already collected.
const teardownTimeout = 2 * time.Second

// Prober runs batched RCPT sessions. It is safe for concurrent use.
type Prober struct {
	opts Options
}

// New returns a Prober. It never returns an error: every option has a default.
func New(opts Options) *Prober { return &Prober{opts: opts} }

func ptrBool(b bool) *bool { return &b }

// Probe asks one MX about every address in the request.
//
// The session is connect → EHLO → MAIL FROM → RCPT × N → (one bogus RCPT when
// catch-all detection is asked for) → RSET → QUIT. **DATA is never sent**
// (invariant 8): the probe asks the question and disconnects.
func (p *Prober) Probe(ctx context.Context, req Request) (Response, error) {
	if req.MXHost == "" {
		return Response{}, errors.New("prober: mx_host is required")
	}
	if len(req.Emails) == 0 {
		return Response{}, errors.New("prober: emails is empty")
	}

	out := Response{Results: make(map[string]Result, len(req.Emails))}

	// Refuse the forgotten before anything else — before the guard, before the
	// budget, before a socket could exist (invariant 9).
	emails := req.Emails
	if p.opts.Suppress != nil && p.opts.Suppress.Enabled() {
		var allowed []string
		for _, addr := range emails {
			hit, reason, err := p.opts.Suppress.Suppressed(ctx, addr)
			switch {
			case err != nil:
				// A redundancy that cannot be read is not a reason to stop
				// answering: the authoritative check already ran upstream.
				if p.opts.OnSuppressionError != nil {
					p.opts.OnSuppressionError(err)
				}
				allowed = append(allowed, addr)
			case hit:
				out.Results[addr] = Result{
					Connected: ptrBool(false),
					Class:     ClassSuppressed,
					Err:       reason,
				}
			default:
				allowed = append(allowed, addr)
			}
		}
		if len(allowed) == 0 {
			for _, r := range out.Results {
				p.record(r)
			}
			return out, nil
		}
		emails = allowed
	}

	// A host already known to randomise needs no probing: the verdict travels
	// with the server, so it applies to this domain whether or not anyone has
	// asked about it before.
	known := p.opts.Profiles != nil && p.opts.Profiles.IsRandomiser(ctx, req.MXHost)

	var verdict catchAllVerdict
	if known {
		verdict = catchAllVerdict{catchAll: ptrBool(true), randomiser: ptrBool(true)}
	}

	for i, chunk := range chunks(emails, p.opts.maxRCPT()) {
		// Catch-all is a property of the domain, not of a chunk: establish it
		// once and apply the answer to every address.
		probeCatchAll := req.NeedCatchAll && i == 0 && !known
		results, v := p.session(ctx, req, chunk, probeCatchAll)
		if probeCatchAll {
			verdict = v
		}
		for addr, r := range results {
			out.Results[addr] = r
		}
	}

	if verdict.randomiser != nil && *verdict.randomiser && !known && p.opts.Profiles != nil {
		p.opts.Profiles.MarkRandomiser(ctx, req.MXHost)
	}
	if verdict.catchAll != nil || verdict.randomiser != nil {
		for addr, r := range out.Results {
			r.CatchAll, r.Randomiser = verdict.catchAll, verdict.randomiser
			out.Results[addr] = r
		}
	}
	for _, r := range out.Results {
		p.record(r)
	}
	return out, nil
}

// catchAllVerdict is what the bogus probes established about this domain and
// the server behind it.
type catchAllVerdict struct {
	catchAll   *bool
	randomiser *bool
}

// session runs one SMTP dialogue over one connection.
func (p *Prober) session(ctx context.Context, req Request, addrs []string, probeCatchAll bool) (map[string]Result, catchAllVerdict) {
	out := make(map[string]Result, len(addrs))

	// fail records the same non-answer for every address in the chunk. It is
	// the shape invariant 1 demands: a refusal of *us* is never a statement
	// about a mailbox, so Accepted stays nil and Connected is false.
	fail := func(class Class, code int, reply, errText string) (map[string]Result, catchAllVerdict) {
		hint := retryHint(class, reply, p.opts.deferralRetry())
		for _, a := range addrs {
			out[a] = Result{
				Connected:         ptrBool(false),
				Class:             class,
				SMTPCode:          code,
				EnhancedCode:      EnhancedCode(reply),
				Reply:             reply,
				Err:               errText,
				RetryAfterSeconds: hint,
			}
		}
		return out, catchAllVerdict{}
	}

	// Before anything else: if this node's IP is burned, probing produces no
	// answers worth having and makes the listing worse.
	if p.opts.Health != nil {
		if burned, why := p.opts.Health.Burned(); burned {
			return fail(ClassIPBurned, 0, "", "sending IP stood down: "+why)
		}
	}

	// Resolve and vet before anything else. The address handed to the dialer is
	// an IP literal, so no second, unguarded lookup can happen underneath us
	// (invariant 2).
	//
	// This runs *before* the budget on purpose. A refusal we make ourselves
	// must be free and must touch no shared state: taking a token first would
	// spend a recipient MX's budget on a server we will never contact, and —
	// because mx_host is attacker-influenced — would create a bucket key named
	// after whatever the caller sent.
	ips, err := p.opts.resolveVia().Resolve(ctx, req.MXHost)
	if err != nil {
		var blocked *resolver.BlockedError
		if errors.As(err, &blocked) || errors.Is(err, resolver.ErrNoRoutableAddress) {
			return fail(ClassGuarded, 0, "", err.Error())
		}
		return fail(classifyNetErr(err), 0, "", err.Error())
	}

	// Budget before the socket. A paused MX or an unreachable bucket means the
	// probe is not sent at all (invariant 5) — the addresses come back
	// unattempted, never as a verdict.
	if err := p.acquire(ctx, req); err != nil {
		results, v := fail(budgetClass(err), 0, "", err.Error())
		applyRetryAfter(results, retryAfterFor(err, 0, "", p.opts.deferralRetry()))
		return results, v
	}

	conn, err := p.dialAny(ctx, ips)
	if err != nil {
		return fail(classifyNetErr(err), 0, "", err.Error())
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(p.opts.timeout()))
	}
	r := bufio.NewReader(conn)

	var netErr error
	step := func(cmd string) (int, string, bool) {
		if cmd != "" {
			if _, err := io.WriteString(conn, cmd+"\r\n"); err != nil {
				netErr = err
				return 0, "", false
			}
		}
		code, text, err := readReply(r)
		if err != nil {
			netErr = err
			return 0, "", false
		}
		return code, text, true
	}

	// Banner. A 421 here is the provider throttling the connection itself —
	// it arrives before MAIL FROM and can never be a verdict on a mailbox.
	code, text, ok := step("")
	if !ok {
		return fail(classifyNetErr(netErr), 0, "", netErr.Error())
	}
	if code != 220 {
		// A 421 here is the provider throttling the connection itself, and it
		// is exactly the signal the pacer exists to react to.
		class := Classify(code, text)
		p.observe(ctx, req.MXHost, class)
		return fail(class, code, text, "")
	}

	helo := req.Helo
	if helo == "" {
		helo = p.opts.helo()
	}
	if code, text, ok = step("EHLO " + helo); !ok {
		return fail(classifyNetErr(netErr), 0, "", netErr.Error())
	}
	if code != 250 {
		class := Classify(code, text)
		p.observe(ctx, req.MXHost, class)
		return fail(class, code, text, "")
	}

	from := req.MailFrom
	if from == "" {
		from = p.opts.mailFrom()
	}
	if code, text, ok = step("MAIL FROM:<" + from + ">"); !ok {
		return fail(classifyNetErr(netErr), 0, "", netErr.Error())
	}
	if code != 250 {
		// Everything up to and including a failed MAIL FROM is about us.
		class := Classify(code, text)
		p.observe(ctx, req.MXHost, class)
		return fail(class, code, text, "")
	}

	// Only past this point may a 5xx mean "no such mailbox" (invariant 1).
	policyRun, stopped := 0, false
	for i, addr := range addrs {
		// One token per recipient: the band is a rate of questions asked, and
		// batching many RCPTs down one connection must not spend less budget
		// than asking them one at a time would.
		if i > 0 {
			if err := p.acquire(ctx, req); err != nil {
				for _, rest := range addrs[i:] {
					out[rest] = Result{Connected: ptrBool(false), Class: budgetClass(err), Err: err.Error()}
				}
				return out, catchAllVerdict{}
			}
		}
		code, text, ok = step("RCPT TO:<" + addr + ">")
		if !ok {
			// The connection died mid-batch: the addresses already answered
			// keep their answers, the rest are unattempted.
			for _, rest := range addrs[i:] {
				out[rest] = Result{
					Connected: ptrBool(false),
					Class:     classifyNetErr(netErr),
					Err:       netErr.Error(),
				}
			}
			return out, catchAllVerdict{}
		}
		r := rcptResult(code, text, p.opts.deferralRetry())
		p.observe(ctx, req.MXHost, r.Class)
		if r.Class == ClassPolicy && p.opts.Health != nil {
			// A policy reply is about our client. It never moves the pacer
			// (invariant 6); it feeds IP health, and nothing else.
			p.opts.Health.ObservePolicy(req.MXHost)
		}
		out[addr] = r

		// Consecutive, not cumulative: one policy reply among ordinary answers
		// is a per-recipient quirk — a distribution list rejecting external
		// senders, say — not the server refusing the client.
		if r.Class == ClassPolicy {
			policyRun++
		} else {
			policyRun = 0
		}
		if stop := p.opts.policyStop(); stop > 0 && policyRun >= stop {
			reason := fmt.Sprintf("not attempted: %d consecutive policy replies from this server", policyRun)
			for _, rest := range addrs[i+1:] {
				out[rest] = Result{
					Connected: ptrBool(false),
					Class:     ClassPolicy,
					Err:       reason,
				}
			}
			// A server refusing us cannot tell us which local parts exist, so
			// the catch-all probes are pointless too.
			stopped = true
			break
		}
	}

	var verdict catchAllVerdict
	if probeCatchAll && !stopped {
		accepted, answered := 0, 0
		for range p.opts.catchAllProbes() {
			bogus, err := bogusAddress(req.Domain)
			if err != nil {
				break
			}
			// A question asked is budget spent, bogus or not.
			if err := p.acquire(ctx, req); err != nil {
				break
			}
			code, text, ok = step("RCPT TO:<" + bogus + ">")
			if !ok {
				break
			}
			answered++
			if Classify(code, text) == ClassValid {
				accepted++
			}
		}
		verdict = decideCatchAll(accepted, answered)
	}

	// RSET abandons the transaction explicitly rather than leaving a bare QUIT
	// after RCPTs, which reads as an aborted delivery attempt in a server log.
	//
	// Both writes are best-effort and get their own short deadline: the answers
	// are already in hand, and a tarpitting server that stops reading must not
	// be able to hold the session — and its slot in the rate budget — open for
	// the remainder of the session timeout.
	_ = conn.SetWriteDeadline(time.Now().Add(teardownTimeout))
	_, _ = io.WriteString(conn, "RSET\r\n")
	_, _ = io.WriteString(conn, "QUIT\r\n")
	return out, verdict
}

// decideCatchAll reads the bogus probes.
//
//   - every one accepted  → the domain takes anything; no 250 here means a thing
//   - every one rejected  → the server answers honestly and the real replies stand
//   - anything in between → the server is answering by coin flip. That is a fact
//     about the *host*, so it condemns every domain behind it. CatchAll is set
//     too: it is the conservative reading, and it is the field callers that do
//     not yet understand Randomiser already handle correctly.
func decideCatchAll(accepted, answered int) catchAllVerdict {
	switch {
	case answered == 0:
		return catchAllVerdict{}
	case accepted == answered:
		return catchAllVerdict{catchAll: ptrBool(true), randomiser: ptrBool(false)}
	case accepted == 0:
		return catchAllVerdict{catchAll: ptrBool(false), randomiser: ptrBool(false)}
	default:
		return catchAllVerdict{catchAll: ptrBool(true), randomiser: ptrBool(true)}
	}
}

// acquire asks the pacer for budget. A nil pacer means unpaced, which only a
// unit test against a fake server may do.
func (p *Prober) acquire(ctx context.Context, req Request) error {
	if p.opts.Pacer == nil {
		return nil
	}
	return p.opts.Pacer.Acquire(ctx, req.MXHost, req.Domain)
}

// observe reports one answer to the pacer, reduced to the only question it is
// allowed to ask: was this a genuine rate signal?
func (p *Prober) observe(ctx context.Context, mxHost string, class Class) {
	if p.opts.Pacer == nil {
		return
	}
	p.opts.Pacer.Observe(ctx, mxHost, class.IsThrottle())
}

// record reports one finished result. Blocked reasons are kept apart from
// answers: a refusal to send is an operational fact, not a measurement of a
// mailbox, and an operator alerts on them differently.
func (p *Prober) record(r Result) {
	m := p.opts.Metrics
	if m == nil {
		return
	}
	class := string(r.Class)
	m.Result(class)
	m.Reply(r.SMTPCode, class)
	switch r.Class {
	case ClassGuarded, ClassNoBudget, ClassPaused, ClassIPBurned, ClassSuppressed:
		m.Blocked(class)
	case ClassPolicy:
		if strings.HasPrefix(r.Err, "not attempted:") {
			m.Blocked("policy_stop")
		}
	}
}

// budgetClass separates "we stood this MX down" from "we could not establish a
// budget at all"; plan 009 counts them apart, one being normal operation and
// the other an incident.
func budgetClass(err error) Class {
	if errors.Is(err, pacer.ErrPaused) {
		return ClassPaused
	}
	return ClassNoBudget
}

// dialAny tries the vetted addresses in order, as a real sender does, and
// returns the first connection it gets.
func (p *Prober) dialAny(ctx context.Context, addrs []netip.Addr) (net.Conn, error) {
	var lastErr error
	for _, a := range addrs {
		target := net.JoinHostPort(a.String(), p.opts.port())
		conn, err := p.opts.dialer().DialContext(ctx, p.opts.network(), target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("prober: no address to dial")
	}
	return nil, lastErr
}

// rcptResult turns one RCPT reply into a Result. Only ClassValid and
// ClassInvalid are statements about the mailbox; every other class means the
// question was not answered, so Accepted stays nil.
func rcptResult(code int, text string, deferralRetry time.Duration) Result {
	class := Classify(code, text)
	r := Result{
		Connected:         ptrBool(true),
		Class:             class,
		SMTPCode:          code,
		EnhancedCode:      EnhancedCode(text),
		Reply:             text,
		RetryAfterSeconds: retryHint(class, text, deferralRetry),
	}
	switch class {
	case ClassValid:
		r.Accepted = ptrBool(true)
	case ClassInvalid:
		r.Accepted = ptrBool(false)
	}
	return r
}

// bogusAddress builds a local part no real mailbox can be, for catch-all
// detection. It is random rather than a fixed string so a server cannot learn
// to answer probes differently from ordinary traffic.
func bogusAddress(domain string) (string, error) {
	if domain == "" {
		return "", errors.New("prober: domain is required for catch-all detection")
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("prober: random local part: %w", err)
	}
	return hex.EncodeToString(b[:]) + "@" + strings.TrimSuffix(domain, "."), nil
}

func chunks[T any](s []T, n int) [][]T {
	var out [][]T
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}
