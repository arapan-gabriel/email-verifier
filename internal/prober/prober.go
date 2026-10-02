package prober

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
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
	// Enforcing, not Enabled: a list can be configured and loaded without yet
	// refusing anything, which is the state a node is in until its first real
	// export has been checked.
	Enforcing() bool
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

// SessionLeaser bounds how many SMTP sessions are open at once per receiving
// system (plan 028). *pacer.Pacer implements it; a Pacer that does not is
// unbounded in concurrency, which only a unit test may accept — main asserts
// the production pacer satisfies it at compile time.
type SessionLeaser interface {
	AcquireSession(ctx context.Context, mxHost, domain string) (release func(), waited time.Duration, err error)
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

// TLSProfiles remembers hosts whose STARTTLS cannot be completed by this
// client (plan 031). Optional on top of Profiles: a Profiles that does not
// implement it means every session tries TLS, which is today's behaviour.
// Both calls are best effort — a store failure reads as "not broken", so the
// session tries TLS and falls back if it must.
type TLSProfiles interface {
	TLSBroken(ctx context.Context, mxHost string) bool
	MarkTLSBroken(ctx context.Context, mxHost string)
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
	// SourceIP is the address this node connects from. With Helo and the
	// MailFrom domain it makes up the Identity the classifier recognises in a
	// refusal (plan 030). Optional: without it only the names are matched.
	SourceIP string
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

	// OnReply is called once per result whose class is neither valid nor
	// invalid — the verdicts somebody will later have to explain. The event
	// arrives already redacted and truncated (see reply.go), so a careless
	// wiring cannot put a recipient into a log line. Nil means nobody is
	// listening.
	OnReply func(ReplyEvent)

	// OnRefusal is called with every ClassPolicy reply — at RCPT and before it —
	// with the real MX host and the reply as read. It is how a stand-down
	// (plan 032) sees refusals of us; the pacer never does (invariant 6). Nil
	// means nobody is listening.
	OnRefusal func(mxHost, reply string)

	// OnLease is called once per session lease granted, with how long the
	// session waited for it (plan 028). Nil means nobody is listening.
	OnLease func(mxHost string, waited time.Duration)

	// MultiDomain says whether the receiving system behind an MX host may be
	// asked about several domains in one session, and names its family (plan
	// 033; pacer.MultiDomain). Nil means never: a `Domains` request is then
	// served as one session per domain, correct and just not faster.
	MultiDomain func(mxHost string) (family string, ok bool)

	// OnFallback is called once per family, the first time a receiver refuses
	// a foreign domain as relay and grouping is switched off for that family on
	// this node until restart (plan 033). The reply arrives redacted. Nil means
	// nobody is listening.
	OnFallback func(family, mxHost, reply string)

	// OnPaced is called once per token the pacer granted — one per question
	// asked, real or catch-all — with the real MX host. A 250 is never logged
	// by OnReply, so without this the rate a receiving system actually saw
	// cannot be read back from the journal (plan 026's node gate). Nil means
	// nobody is listening.
	OnPaced func(mxHost string)

	// ReplyMaxChars caps the logged reply. Zero uses DefaultReplyMaxChars; a
	// negative value means no cap.
	ReplyMaxChars int
	// DeferralRetry is the retry hint given when the server offers none.
	DeferralRetry time.Duration
	// PolicyStopMax bounds what a single request may raise PolicyStop to. Zero
	// means requests may lower the ceiling but never raise it.
	PolicyStopMax int

	// PolicyStop is how many *consecutive* ClassPolicy replies end a session.
	// Zero disables it. A server that refuses the client refuses it for the
	// whole session, so every remaining RCPT spends a token on a question whose
	// answer is already known — and keeps hammering a server that has just told
	// us to go away.
	PolicyStop int
	// CatchAllProbes is the ceiling on known-bad local parts asked per domain
	// (plan 029). They are asked one at a time and the sequence stops at the
	// first rejection: a rejected bogus address settles "answers honestly". Only
	// an accepted one earns the next question, because one 250 cannot tell a
	// catch-all from a host answering by coin flip; a second can.
	CatchAllProbes int
	// CatchAllAuditRate is the share of catch-all questions asked with the full
	// sequence of auditProbes regardless of the answers (plan 029), so the rate
	// of randomisers stopping early would miss stays measured, not assumed.
	CatchAllAuditRate float64
	// Rand draws a float in [0, 1) for the audit sample. Nil uses math/rand/v2;
	// tests inject their own.
	Rand func() float64

	// StartTLS is "opportunistic" (upgrade when the server advertises STARTTLS,
	// the default) or "off" (plaintext always — the escape hatch). Plan 031.
	StartTLS string
	// TLSHandshakeTimeout bounds the handshake inside the session deadline.
	// Zero uses defaultTLSHandshakeTimeout.
	TLSHandshakeTimeout time.Duration
	// TLSRootCAs verifies the server's certificate *for the record* — the
	// outcome is reported, never enforced. Nil uses the host's roots; tests
	// inject a pool to exercise "verified".
	TLSRootCAs *x509.CertPool
}

// defaultTLSHandshakeTimeout bounds a STARTTLS handshake when none is set.
const defaultTLSHandshakeTimeout = 10 * time.Second

// The TLS outcomes a session reports on its results and in the metric (plan
// 031). Bounded.
const (
	TLSNone       = "none"
	TLSVerified   = "verified"
	TLSUnverified = "unverified"
	TLSFailed     = "failed"
	// TLSFallback: the server offered STARTTLS and the upgrade failed — the
	// handshake (Jimdo offers only DHE suites Go does not implement), a dropped
	// connection, or a refusal of the command — and the session was answered in
	// plaintext, as Postfix does at security level `may` (plan 031, 2026-10-02).
	TLSFallback = "fallback"
	// TLSSkipped: the host is remembered as TLS-broken (`mx:<host>:tls_broken`),
	// so this session did not try and went plaintext from the start.
	TLSSkipped = "skipped"
)

// TLSRecorder counts how sessions were encrypted (plan 031). Optional on top
// of Recorder, like CatchAllRecorder.
type TLSRecorder interface {
	TLSSession(outcome string)
}

// MultiDomainRecorder counts how many domains each session asked about and
// every family that fell back to one domain per session (plan 033). Optional
// on top of Recorder, like TLSRecorder.
type MultiDomainRecorder interface {
	SessionDomains(n int)
	MultiDomainFallback(family string)
}

// auditProbes is the sequence an audit sample asks: the length every domain
// was asked before plan 029, so audited outcomes compare with the old record.
const auditProbes = 3

// CatchAllRecorder counts how each domain's catch-all question was settled.
// Optional on top of Recorder: a metrics sink that does not implement it simply
// does not get the count.
type CatchAllRecorder interface {
	CatchAllProbes(outcome string)
}

// The bounded outcomes of a catch-all question (plan 029).
const (
	catchAllSkippedNoAccept = "skipped_no_accept"
	catchAllCleanAfter1     = "clean_after_1"
	catchAllCatchAll        = "catch_all"
	catchAllRandomiser      = "randomiser"
	catchAllAuditFull       = "audit_full"
	catchAllUnanswered      = "unanswered"
)

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

// policyStopMax is the ceiling a request may raise its own limit to. Zero means
// the configured default is also the maximum, so a caller can lower but never
// raise.
func (o Options) policyStopMax() int { return o.PolicyStopMax }

// policyStopFor resolves the ceiling for one request.
//
// **Clamped, never rejected.** An over-ambitious value is not a malformed
// request, and answering it with a 400 would reach the caller as a transport
// failure — every address in the batch a non-answer (their invariant: a failure
// to reach a conclusion is never a verdict). Quietly using a slightly lower
// ceiling is the failure that costs least, so the bound is enforced here rather
// than taught to the caller.
//
// A value of 1 is raised to 2 for the same reason config refuses it: a single
// 5.7.x can be a per-recipient policy, and stopping on it would throw away the
// rest of the batch on one server's opinion of one address.
func (o Options) policyStopFor(req Request) int {
	want := req.PolicyStop
	if want <= 0 {
		return o.policyStop()
	}
	if want == 1 {
		want = 2
	}
	ceiling := o.policyStopMax()
	if ceiling <= 0 {
		ceiling = o.policyStop()
	}
	if want > ceiling {
		return ceiling
	}
	return want
}

func (o Options) catchAllProbes() int {
	if o.CatchAllProbes > 0 {
		return o.CatchAllProbes
	}
	return 2
}

func (o Options) auditDrawn() bool {
	if o.CatchAllAuditRate <= 0 {
		return false
	}
	draw := o.Rand
	if draw == nil {
		draw = mathrand.Float64
	}
	return draw() < o.CatchAllAuditRate
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
	// skipTLS is set on the one plaintext rerun after a failed upgrade, so the
	// rerun cannot try TLS again and the fallback cannot loop. Unexported: it is
	// never part of the HTTP contract.
	skipTLS bool
	// PolicyStop overrides the configured ceiling for this request only, and is
	// clamped by the prober (plan 017).
	//
	// The caller knows what shape its question has and this service cannot. A
	// finder's candidate ladder is a list of guesses, and every wrong guess at
	// a Microsoft tenant answers `550 5.4.1 Access denied` — so the default
	// ceiling of five ends the session one candidate before a six-rung ladder
	// finishes, at every M365 tenant, every time. A verification batch of real
	// addresses has no such shape and wants the default.
	//
	// Zero means "use the configured default".
	PolicyStop int
	// Domains, when set, replaces Domain/Emails/NeedCatchAll with several
	// domains answered by the same MX (plan 033). Whether they share a session
	// is the prober's decision (Options.MultiDomain); the response is the same
	// shape either way.
	Domains []DomainGroup
}

// DomainGroup is one domain's addresses in a multi-domain request (plan 033).
type DomainGroup struct {
	Domain       string
	Emails       []string
	NeedCatchAll bool
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
	// TLS is how the session that produced this result was encrypted (plan
	// 031): "none" (the server did not offer STARTTLS, or it is off),
	// "verified" (upgraded, and the certificate chains to a trusted root for
	// the MX name), "unverified" (upgraded, certificate not trusted or not for
	// this name — recorded, never enforced, as an opportunistic MTA does),
	// "fallback" (offered but refused or the handshake failed; answered in
	// plaintext), "skipped" (host remembered as TLS-broken, plaintext without
	// asking) or "failed" (handshake and plaintext retry both failed). Empty
	// when no session was opened at all.
	TLS string `json:"tls,omitempty"`
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
	// fallen holds the families a receiver refused a foreign domain for (plan
	// 033): grouping stays off for them on this node until restart.
	fallen sync.Map
}

// New returns a Prober. It never returns an error: every option has a default.
func New(opts Options) *Prober { return &Prober{opts: opts} }

// identity is who this node is on the wire, for the classifier (plan 030).
func (p *Prober) identity() Identity {
	_, domain, _ := strings.Cut(p.opts.MailFrom, "@")
	return Identity{SourceIP: p.opts.SourceIP, Helo: p.opts.Helo, MailFromDomain: domain}
}

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
	if len(req.Domains) > 0 {
		return p.probeDomains(ctx, req)
	}
	if len(req.Emails) == 0 {
		return Response{}, errors.New("prober: emails is empty")
	}

	out := Response{Results: make(map[string]Result, len(req.Emails))}

	// Refuse the forgotten before anything else — before the guard, before the
	// budget, before a socket could exist (invariant 9).
	emails := p.unsuppressed(ctx, req.Emails, out.Results)
	if len(emails) == 0 {
		for _, r := range out.Results {
			p.record(req.MXHost, r)
		}
		return out, nil
	}

	// A host already known to randomise needs no probing: the verdict travels
	// with the server, so it applies to this domain whether or not anyone has
	// asked about it before.
	known := p.opts.Profiles != nil && p.opts.Profiles.IsRandomiser(ctx, req.MXHost)

	var verdict catchAllVerdict
	if known {
		verdict = catchAllVerdict{catchAll: ptrBool(true), randomiser: ptrBool(true)}
	}

	settled := known
	for _, chunk := range chunks(emails, p.opts.maxRCPT()) {
		// Catch-all is a property of the domain, not of a chunk: establish it
		// once and apply the answer to every address. It is asked in the first
		// session that had a 250 to qualify (plan 029) — before, only chunk 0
		// could ask, so a 250 in a later chunk never got a verdict.
		probeCatchAll := req.NeedCatchAll && !settled
		results, vs, _ := p.session(ctx, req, []sessionGroup{{domain: req.Domain, addrs: chunk, askCatchAll: probeCatchAll}}, false)
		v := vs[req.Domain]
		if probeCatchAll && (v.catchAll != nil || v.randomiser != nil) {
			verdict, settled = v, true
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
		p.record(req.MXHost, r)
	}
	return out, nil
}

// unsuppressed returns the addresses that may be contacted and records a
// ClassSuppressed result in into for each one that may not (invariant 9).
func (p *Prober) unsuppressed(ctx context.Context, emails []string, into map[string]Result) []string {
	if p.opts.Suppress == nil || !p.opts.Suppress.Enforcing() {
		return emails
	}
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
			into[addr] = Result{
				Connected: ptrBool(false),
				Class:     ClassSuppressed,
				Err:       reason,
			}
		default:
			allowed = append(allowed, addr)
		}
	}
	return allowed
}

// sessionGroup is one domain's addresses inside one session.
type sessionGroup struct {
	domain      string
	addrs       []string
	askCatchAll bool
}

// probeDomains serves a multi-domain request (plan 033). A family cleared for
// it (Options.MultiDomain) gets whole domains packed into sessions up to
// MaxRCPTPerSession; any other host gets one session per domain — the same
// sessions, leases and tokens three separate requests would have taken. The
// catch-all question is per domain, and a randomiser verdict, being about the
// server, condemns every domain in the request (invariant 7).
func (p *Prober) probeDomains(ctx context.Context, req Request) (Response, error) {
	groups := mergeGroups(req.Domains)
	out := Response{Results: map[string]Result{}}
	domainOf := map[string]string{}
	need := map[string]bool{}
	var queue []sessionGroup
	for _, g := range groups {
		for _, a := range g.Emails {
			domainOf[a] = g.Domain
		}
		need[g.Domain] = g.NeedCatchAll
		if allowed := p.unsuppressed(ctx, g.Emails, out.Results); len(allowed) > 0 {
			queue = append(queue, sessionGroup{domain: g.Domain, addrs: allowed})
		}
	}
	if len(queue) == 0 {
		if len(out.Results) == 0 {
			return Response{}, errors.New("prober: emails is empty")
		}
		for _, r := range out.Results {
			p.record(req.MXHost, r)
		}
		return out, nil
	}

	known := p.opts.Profiles != nil && p.opts.Profiles.IsRandomiser(ctx, req.MXHost)
	verdicts := map[string]catchAllVerdict{}
	for len(queue) > 0 {
		_, grouped := p.grouping(req.MXHost)
		var batch []sessionGroup
		batch, queue = takeSession(queue, p.opts.maxRCPT(), grouped)
		for i := range batch {
			_, settled := verdicts[batch[i].domain]
			batch[i].askCatchAll = need[batch[i].domain] && !known && !settled
		}
		sreq := req
		sreq.Domain, sreq.Domains = batch[0].domain, nil
		results, vs, rest := p.session(ctx, sreq, batch, len(batch) > 1)
		for addr, r := range results {
			out.Results[addr] = r
		}
		for d, v := range vs {
			if v.catchAll != nil || v.randomiser != nil {
				verdicts[d] = v
			}
		}
		// What a receiver refused to relay goes again, one domain per session:
		// the fallback is already in force.
		queue = append(regroup(rest, domainOf), queue...)
	}

	var condemned *catchAllVerdict
	if known {
		condemned = &catchAllVerdict{catchAll: ptrBool(true), randomiser: ptrBool(true)}
	}
	for _, v := range verdicts {
		if v.randomiser != nil && *v.randomiser {
			v := v
			condemned = &v
			if !known && p.opts.Profiles != nil {
				p.opts.Profiles.MarkRandomiser(ctx, req.MXHost)
			}
			break
		}
	}
	for addr, r := range out.Results {
		v, ok := verdicts[domainOf[addr]]
		if condemned != nil {
			v, ok = *condemned, true
		}
		if ok {
			r.CatchAll, r.Randomiser = v.catchAll, v.randomiser
			out.Results[addr] = r
		}
	}
	for _, r := range out.Results {
		p.record(req.MXHost, r)
	}
	return out, nil
}

// mergeGroups folds groups naming the same domain into one: two callers'
// batches can meet in one request. Order is kept; an address is asked once.
func mergeGroups(in []DomainGroup) []DomainGroup {
	var out []DomainGroup
	at := map[string]int{}
	seen := map[string]bool{}
	for _, g := range in {
		d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(g.Domain), "."))
		i, ok := at[d]
		if !ok {
			i = len(out)
			at[d] = i
			out = append(out, DomainGroup{Domain: g.Domain})
		}
		out[i].NeedCatchAll = out[i].NeedCatchAll || g.NeedCatchAll
		for _, e := range g.Emails {
			if !seen[e] {
				seen[e] = true
				out[i].Emails = append(out[i].Emails, e)
			}
		}
	}
	return out
}

// takeSession takes the next session's groups off the queue: whole domains up
// to limit addresses when grouped, otherwise one domain. A domain larger than
// limit is split, as a single-domain request always was.
func takeSession(queue []sessionGroup, limit int, grouped bool) (batch, rest []sessionGroup) {
	first := queue[0]
	if len(first.addrs) > limit {
		head := sessionGroup{domain: first.domain, addrs: first.addrs[:limit]}
		queue[0] = sessionGroup{domain: first.domain, addrs: first.addrs[limit:]}
		return []sessionGroup{head}, queue
	}
	batch, queue = []sessionGroup{first}, queue[1:]
	total := len(first.addrs)
	for grouped && len(queue) > 0 && total+len(queue[0].addrs) <= limit {
		total += len(queue[0].addrs)
		batch, queue = append(batch, queue[0]), queue[1:]
	}
	return batch, queue
}

// regroup turns unasked addresses back into per-domain groups, in order.
func regroup(addrs []string, domainOf map[string]string) []sessionGroup {
	var out []sessionGroup
	for _, a := range addrs {
		d := domainOf[a]
		if n := len(out); n > 0 && out[n-1].domain == d {
			out[n-1].addrs = append(out[n-1].addrs, a)
			continue
		}
		out = append(out, sessionGroup{domain: d, addrs: []string{a}})
	}
	return out
}

// grouping reports whether mxHost's family may be asked about several domains
// in one session right now: cleared in families.json and not fallen back.
func (p *Prober) grouping(mxHost string) (string, bool) {
	if p.opts.MultiDomain == nil {
		return "", false
	}
	family, ok := p.opts.MultiDomain(mxHost)
	if !ok {
		return family, false
	}
	if _, fell := p.fallen.Load(family); fell {
		return family, false
	}
	return family, true
}

// fallBack switches grouping off for mxHost's family on this node, once.
func (p *Prober) fallBack(mxHost, reply string) {
	family, _ := p.grouping(mxHost)
	if family == "" {
		return
	}
	if _, already := p.fallen.LoadOrStore(family, true); already {
		return
	}
	if rec, ok := p.opts.Metrics.(MultiDomainRecorder); ok {
		rec.MultiDomainFallback(family)
	}
	if p.opts.OnFallback != nil {
		limit := p.opts.ReplyMaxChars
		if limit == 0 {
			limit = DefaultReplyMaxChars
		}
		p.opts.OnFallback(family, mxHost, redactReply(reply, limit))
	}
}

// relayRe matches a receiver refusing to answer for a domain it does not host:
// Postfix's "Relay access denied", Exchange's "5.7.64 TenantAttribution; Relay
// Access Denied", Sendmail's "Relaying denied", Exim's "relay not permitted".
var relayRe = regexp.MustCompile(`(?i)relay(ing)? (access )?(denied|not permitted|not allowed|prohibited)|` +
	`unable to relay|not permitted to relay|we do not relay|5\.7\.64`)

// relayRefusal reports whether a RCPT reply refused a foreign domain as relay.
func relayRefusal(code int, text string) bool {
	return code >= 400 && relayRe.MatchString(text)
}

// catchAllVerdict is what the bogus probes established about this domain and
// the server behind it.
type catchAllVerdict struct {
	catchAll   *bool
	randomiser *bool
}

// session runs one SMTP dialogue over one connection, about one domain's
// addresses or — grouped, plan 033 — several domains' in turn. It returns the
// results, each asked domain's catch-all verdict, and the addresses it left
// unasked because the receiver refused a foreign domain as relay; those go
// again in sessions of their own.
func (p *Prober) session(ctx context.Context, req Request, groups []sessionGroup, grouped bool) (map[string]Result, map[string]catchAllVerdict, []string) {
	var addrs []string
	var groupOf []int
	for gi, g := range groups {
		addrs = append(addrs, g.addrs...)
		for range g.addrs {
			groupOf = append(groupOf, gi)
		}
	}
	out := make(map[string]Result, len(addrs))

	// How this session was encrypted (plan 031), stamped on every result it
	// produced once it reached a server. Empty while no session exists, so our
	// own refusals before the socket carry no TLS field at all.
	tlsOutcome := ""
	defer func() {
		if tlsOutcome == "" {
			return
		}
		for a, r := range out {
			r.TLS = tlsOutcome
			out[a] = r
		}
		if rec, ok := p.opts.Metrics.(TLSRecorder); ok {
			rec.TLSSession(tlsOutcome)
		}
		if rec, ok := p.opts.Metrics.(MultiDomainRecorder); ok {
			rec.SessionDomains(len(groups))
		}
	}()

	// fail records the same non-answer for every address in the chunk. It is
	// the shape invariant 1 demands: a refusal of *us* is never a statement
	// about a mailbox, so Accepted stays nil and Connected is false.
	fail := func(class Class, code int, reply, errText string) (map[string]Result, map[string]catchAllVerdict, []string) {
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
		return out, nil, nil
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

	// One session per receiving system at a time (plan 028), counted centrally
	// and held for the whole session — the real RCPTs and plan 029's catch-all
	// question alike. Taken after the resolve (a refusal we make ourselves is
	// free) and before the token and the socket. Fails closed, and a wait past
	// the bound comes back unattempted with a retry hint, never a verdict.
	leaseRelease, err := p.leaseSession(ctx, req)
	if err != nil {
		results, v, rest := fail(budgetClass(err), 0, "", err.Error())
		applyRetryAfter(results, retryAfterFor(err, 0, "", p.opts.deferralRetry()))
		return results, v, rest
	}
	// Idempotent: the TLS fallback below releases early, before it reruns the
	// session under a lease of its own — with one session per receiving system
	// (plan 028), holding this one while asking for the next would wait on
	// ourselves until lease_wait ran out.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(leaseRelease) }
	defer release()

	// Budget before the socket. A paused MX or an unreachable bucket means the
	// probe is not sent at all (invariant 5) — the addresses come back
	// unattempted, never as a verdict.
	if err := p.acquire(ctx, req); err != nil {
		results, v, rest := fail(budgetClass(err), 0, "", err.Error())
		applyRetryAfter(results, retryAfterFor(err, 0, "", p.opts.deferralRetry()))
		return results, v, rest
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
		class := p.beforeRCPT(code, text)
		p.observe(ctx, req.MXHost, class)
		p.refused(req.MXHost, class, text)
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
		class := p.beforeRCPT(code, text)
		p.observe(ctx, req.MXHost, class)
		p.refused(req.MXHost, class, text)
		return fail(class, code, text, "")
	}

	// STARTTLS when offered (plan 031). The upgrade runs over the connection
	// already dialled to a vetted address — no new dial, no new lookup
	// (invariant 2) — and the EHLO is repeated over TLS, because RFC 3207
	// discards the pre-TLS extension list. A refused STARTTLS carries on in
	// plaintext; a failed handshake is answered by one plaintext rerun and the
	// host is remembered (`mx:<host>:tls_broken`). Only a rerun that cannot
	// reach the server is ClassTLSFailed: about us, never a verdict, never a
	// throttle.
	tlsOutcome = TLSNone
	switch {
	case req.skipTLS:
		tlsOutcome = TLSFallback
	case !p.opts.startTLS() || !advertisesSTARTTLS(text):
		// Not offered, or switched off: plaintext, as before plan 031.
	case p.tlsBroken(ctx, req.MXHost):
		tlsOutcome = TLSSkipped
	default:
		code, _, ok = step("STARTTLS")
		if ok && code != 220 {
			// Refused, and the connection is still in its plaintext state:
			// RFC 3207 §4 lets the client carry on without TLS, and that is
			// what an opportunistic MTA does. No reconnect, no memory — the
			// next session asks again.
			tlsOutcome = TLSFallback
			break
		}
		var upgraded net.Conn
		var verified bool
		var upErr error
		if ok {
			upgraded, verified, upErr = p.upgrade(ctx, conn, req.MXHost)
		} else {
			upErr = netErr
		}
		if upErr != nil {
			// The handshake failed or the connection went with it. The
			// session is unusable either way, so — once — answer the request
			// in plaintext on a fresh connection, and remember the host so
			// the next session does not pay for the attempt (Jimdo, 2026-10-02:
			// 32 sessions `tls_failed` that used to be verdicts). Only if that
			// rerun cannot reach the server either does the request end
			// `tls_failed`.
			p.markTLSBroken(ctx, req.MXHost)
			_ = conn.Close()
			release()
			tlsOutcome = "" // this session records nothing; the rerun does
			plain := req
			plain.skipTLS = true
			res, vs, rest := p.session(ctx, plain, groups, grouped)
			failed := false
			for a, rr := range res {
				if rr.Connected == nil || !*rr.Connected {
					if rr.Class == "" || rr.Class == ClassConnError || rr.Class == ClassTimeout {
						rr.Class = ClassTLSFailed
						rr.Err = "tls handshake: " + upErr.Error() + "; plaintext retry: " + rr.Err
						rr.TLS = TLSFailed
						rr.RetryAfterSeconds = retryHint(ClassTLSFailed, "", p.opts.deferralRetry())
						res[a] = rr
						failed = true
					}
				}
			}
			if rec, ok := p.opts.Metrics.(TLSRecorder); ok && failed {
				rec.TLSSession(TLSFailed)
			}
			return res, vs, rest
		}
		conn, r = upgraded, bufio.NewReader(upgraded)
		tlsOutcome = TLSUnverified
		if verified {
			tlsOutcome = TLSVerified
		}
		if code, text, ok = step("EHLO " + helo); !ok {
			return fail(classifyNetErr(netErr), 0, "", netErr.Error())
		}
		if code != 250 {
			class := p.beforeRCPT(code, text)
			p.observe(ctx, req.MXHost, class)
			p.refused(req.MXHost, class, text)
			return fail(class, code, text, "")
		}
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
		class := p.beforeRCPT(code, text)
		p.observe(ctx, req.MXHost, class)
		p.refused(req.MXHost, class, text)
		return fail(class, code, text, "")
	}

	// Only past this point may a 5xx mean "no such mailbox" (invariant 1).
	policyRun, stopped := 0, false
	var relayed []string
	relayStop := false
	for i, addr := range addrs {
		// One token per recipient: the band is a rate of questions asked, and
		// batching many RCPTs down one connection must not spend less budget
		// than asking them one at a time would.
		if i > 0 {
			if err := p.acquire(ctx, req); err != nil {
				for _, rest := range addrs[i:] {
					out[rest] = Result{Connected: ptrBool(false), Class: budgetClass(err), Err: err.Error()}
				}
				return out, nil, nil
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
			return out, nil, nil
		}
		r := rcptResult(code, text, p.opts.deferralRetry(), p.identity())
		// A receiver refusing a domain other than the session's first as relay
		// answered about our question, not the mailbox (invariant 1): policy,
		// never invalid — and not about our IP either, so not IP health.
		relay := grouped && groupOf[i] > 0 && relayRefusal(code, text)
		if relay {
			r.Class, r.Accepted, r.RetryAfterSeconds = ClassPolicy, nil, 0
		}
		p.observe(ctx, req.MXHost, r.Class)
		if r.Class == ClassPolicy && !relay {
			// A policy reply is about our client. It never moves the pacer
			// (invariant 6); it feeds IP health and the stand-down, and
			// nothing else.
			p.refused(req.MXHost, r.Class, text)
			if p.opts.Health != nil {
				p.opts.Health.ObservePolicy(req.MXHost)
			}
		}
		out[addr] = r
		if relay {
			// Asked once: grouping is off for this family from here on, and
			// what is left of this session goes again one domain at a time.
			p.fallBack(req.MXHost, text)
			relayed, relayStop = addrs[i+1:], true
			break
		}

		// Consecutive, not cumulative: one policy reply among ordinary answers
		// is a per-recipient quirk — a distribution list rejecting external
		// senders, say — not the server refusing the client.
		if r.Class == ClassPolicy {
			policyRun++
		} else {
			policyRun = 0
		}
		if stop := p.opts.policyStopFor(req); stop > 0 && policyRun >= stop {
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

	// The catch-all question per domain (plan 029), after every real RCPT.
	// After a relay refusal only the first domain is asked: the receiver has
	// just said it does not answer for the others.
	verdicts := map[string]catchAllVerdict{}
	for gi, g := range groups {
		if !g.askCatchAll || stopped || (relayStop && gi > 0) {
			continue
		}
		verdicts[g.domain] = p.askCatchAll(ctx, req, g.domain, g.addrs, out, func(rcpt string) (int, string, bool) {
			return step("RCPT TO:<" + rcpt + ">")
		})
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
	return out, verdicts, relayed
}

// askCatchAll settles whether this session's 250s mean anything (plan 029).
//
// The question only qualifies an accepted real address, so it is not asked at
// all unless one was accepted in this session: a session of rejections, policy
// refusals (a Microsoft tenant answering 5.4.1 to everything) or deferrals
// leaves both fields nil, and the next session with a 250 asks instead. Bogus
// local parts go one at a time and the sequence stops at the first rejection;
// an accepted one earns the next question, up to catchAllProbes. An audit
// sample asks the full auditProbes regardless of the answers. Every bogus RCPT
// takes a pacer token like any other question (invariant 4), and none of them
// can touch a real address's result (invariant 1): they only feed the verdict.
func (p *Prober) askCatchAll(ctx context.Context, req Request, domain string, addrs []string, out map[string]Result,
	rcpt func(string) (int, string, bool)) catchAllVerdict {
	anyAccepted := false
	for _, addr := range addrs {
		if r, ok := out[addr]; ok && r.Class == ClassValid {
			anyAccepted = true
			break
		}
	}
	if !anyAccepted {
		p.countCatchAll(catchAllSkippedNoAccept)
		return catchAllVerdict{}
	}

	audit := p.opts.auditDrawn()
	limit := p.opts.catchAllProbes()
	if audit {
		limit = max(limit, auditProbes)
	}
	accepted, answered := 0, 0
	for answered < limit {
		bogus, err := bogusAddress(domain)
		if err != nil {
			break
		}
		// A question asked is budget spent, bogus or not.
		if err := p.acquire(ctx, req); err != nil {
			break
		}
		code, text, ok := rcpt(bogus)
		if !ok {
			break
		}
		answered++
		if ClassifyAs(code, text, p.identity()) == ClassValid {
			accepted++
		} else if !audit {
			// A rejected bogus address settles it: either nothing was accepted
			// (an honest server) or something was (a coin flip). Asking again
			// only adds a question to a mailbox that does not exist.
			break
		}
	}

	verdict := decideCatchAll(accepted, answered)
	switch {
	case answered == 0:
		p.countCatchAll(catchAllUnanswered)
	case audit:
		p.countCatchAll(catchAllAuditFull)
	case verdict.randomiser != nil && *verdict.randomiser:
		p.countCatchAll(catchAllRandomiser)
	case verdict.catchAll != nil && *verdict.catchAll:
		p.countCatchAll(catchAllCatchAll)
	default:
		p.countCatchAll(catchAllCleanAfter1)
	}
	return verdict
}

func (p *Prober) countCatchAll(outcome string) {
	if c, ok := p.opts.Metrics.(CatchAllRecorder); ok {
		c.CatchAllProbes(outcome)
	}
}

// beforeRCPT classifies a reply that refused the session before any RCPT was
// asked — the banner, EHLO or MAIL FROM. None of those can be about a mailbox
// (invariant 1), whatever the prose says, but Classify reads prose: Postfix's
// reject_unlisted_sender answers MAIL FROM with "550 5.1.0 <verify@…>: Sender
// address rejected: User unknown in virtual mailbox table", subject 1 plus
// mailbox wording, which Classify calls invalid. fail() stamped that class on
// every address in the chunk, and Data Scout derives `accepted` from `class`,
// so one refusal of our envelope sender recorded a whole batch undeliverable.
// Found by plan 027's session table.
//
// A would-be verdict becomes policy for a 5xx (a permanent refusal of our
// client, which is what ClassPolicy means) and unknown otherwise (a 2xx that is
// not the expected 220/250). Every other class already says who it is about.
func (p *Prober) beforeRCPT(code int, text string) Class {
	c := ClassifyAs(code, text, p.identity())
	if c != ClassValid && c != ClassInvalid {
		return c
	}
	if code >= 500 && code <= 599 {
		return ClassPolicy
	}
	return ClassUnknown
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

// leaseSession takes the session lease when the pacer enforces one. A pacer
// that does not implement SessionLeaser (a unit-test fake) is unbounded.
func (p *Prober) leaseSession(ctx context.Context, req Request) (func(), error) {
	l, ok := p.opts.Pacer.(SessionLeaser)
	if !ok || p.opts.Pacer == nil {
		return func() {}, nil
	}
	release, waited, err := l.AcquireSession(ctx, req.MXHost, req.Domain)
	if err != nil {
		return nil, err
	}
	if p.opts.OnLease != nil {
		p.opts.OnLease(req.MXHost, waited)
	}
	return release, nil
}

// acquire asks the pacer for budget. A nil pacer means unpaced, which only a
// unit test against a fake server may do.
func (p *Prober) acquire(ctx context.Context, req Request) error {
	if p.opts.Pacer == nil {
		return nil
	}
	if err := p.opts.Pacer.Acquire(ctx, req.MXHost, req.Domain); err != nil {
		return err
	}
	if p.opts.OnPaced != nil {
		p.opts.OnPaced(req.MXHost)
	}
	return nil
}

// refused passes a refusal of us to the stand-down hook. Only ClassPolicy:
// throttles and deferrals are rate or recipient signals, and a TLS failure is
// our gap (plan 031), none of them a reputation verdict.
func (p *Prober) refused(mxHost string, class Class, reply string) {
	if class != ClassPolicy || p.opts.OnRefusal == nil {
		return
	}
	p.opts.OnRefusal(mxHost, reply)
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
func (p *Prober) record(mxHost string, r Result) {
	p.explain(mxHost, r)
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

// explain hands a hard-to-read verdict to whoever is logging, with the reply
// redacted here rather than at the caller.
func (p *Prober) explain(mxHost string, r Result) {
	if p.opts.OnReply == nil || !r.Class.explains() {
		return
	}
	limit := p.opts.ReplyMaxChars
	if limit == 0 {
		limit = DefaultReplyMaxChars
	}
	p.opts.OnReply(ReplyEvent{
		MXHost:       mxHost,
		Class:        r.Class,
		SMTPCode:     r.SMTPCode,
		EnhancedCode: r.EnhancedCode,
		Reply:        redactReply(r.Reply, limit),
		Err:          redactReply(r.Err, limit),
		TLS:          r.TLS,
	})
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
func rcptResult(code int, text string, deferralRetry time.Duration, id Identity) Result {
	class := ClassifyAs(code, text, id)
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

// tlsBroken reports whether this host is remembered as one our client cannot
// complete STARTTLS with. Fail-open: no store, or a store error, means "try".
func (p *Prober) tlsBroken(ctx context.Context, mxHost string) bool {
	t, ok := p.opts.Profiles.(TLSProfiles)
	return ok && t.TLSBroken(ctx, mxHost)
}

// markTLSBroken remembers the host; best effort.
func (p *Prober) markTLSBroken(ctx context.Context, mxHost string) {
	if t, ok := p.opts.Profiles.(TLSProfiles); ok {
		t.MarkTLSBroken(ctx, mxHost)
	}
}

func (o Options) startTLS() bool {
	return o.StartTLS != "off"
}

func (o Options) tlsHandshakeTimeout() time.Duration {
	if o.TLSHandshakeTimeout > 0 {
		return o.TLSHandshakeTimeout
	}
	return defaultTLSHandshakeTimeout
}

// advertisesSTARTTLS reports whether an EHLO reply lists the STARTTLS
// extension. The reply is read whole (plan 022); the first line is the
// greeting, every later line one keyword with optional parameters.
func advertisesSTARTTLS(ehlo string) bool {
	for i, line := range strings.Split(ehlo, "\n") {
		if i == 0 || len(line) < 5 {
			continue
		}
		fields := strings.Fields(line[4:])
		if len(fields) > 0 && strings.EqualFold(fields[0], "STARTTLS") {
			return true
		}
	}
	return false
}

// upgrade runs the client side of a STARTTLS handshake over conn, the
// connection already dialled to a vetted address.
//
// The policy is an opportunistic MTA's (RFC 7435; Postfix's
// smtp_tls_security_level = may): SNI is the MX name, TLS 1.2 is the floor and
// 1.3 is offered, and the certificate is checked **for the record only** — most
// MX certificates do not name the MX, and a sender that refused them would
// refuse half the internet. verified reports whether the chain and the name
// check passed.
func (p *Prober) upgrade(ctx context.Context, conn net.Conn, mxHost string) (net.Conn, bool, error) {
	name := strings.TrimSuffix(mxHost, ".")
	verified := false
	roots := p.opts.TLSRootCAs
	cfg := &tls.Config{
		ServerName: name,
		MinVersion: tls.VersionTLS12,
		// Verification is recorded in VerifyConnection, never enforced here.
		InsecureSkipVerify: true, //nolint:gosec // opportunistic STARTTLS, plan 031
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				DNSName: name, Roots: roots, Intermediates: inter,
			})
			verified = err == nil
			return nil
		},
	}
	hctx, cancel := context.WithTimeout(ctx, p.opts.tlsHandshakeTimeout())
	defer cancel()
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		return nil, false, err
	}
	return tlsConn, verified, nil
}
