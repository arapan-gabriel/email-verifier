package prober_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/smtp"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// startWorkspace runs mxsim's Google-like profile (one system, many domains,
// one domain per transaction) and returns its port and engine, whose Stats are
// what the server really saw.
func startWorkspace(t *testing.T, tweak ...func(*policy.Profile)) (string, *policy.Engine) {
	t.Helper()
	return startProfile(t, "google-workspace", tweak...)
}

// startProfile runs the named profile from config/mxsim.
func startProfile(t *testing.T, name string, tweak ...func(*policy.Profile)) (string, *policy.Engine) {
	t.Helper()
	p, err := policy.LoadProfile(filepath.Join("..", "..", "config", "mxsim", name+".yaml"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	for _, f := range tweak {
		f(p)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	eng := policy.NewEngine(p, clock.New())
	srv := smtp.New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		srv.Shutdown()
		<-done
	})
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port, eng
}

// countingPacer grants everything and counts what it was asked for.
type countingPacer struct {
	mu     sync.Mutex
	tokens []string // mx host per token
	leases int
}

func (c *countingPacer) Acquire(_ context.Context, mxHost, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = append(c.tokens, mxHost)
	return nil
}

func (c *countingPacer) Observe(context.Context, string, bool) {}

func (c *countingPacer) AcquireSession(context.Context, string, string) (func(), time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leases++
	return func() {}, 0, nil
}

// mdRecorder is a metrics sink that also counts plan 033's two series.
type mdRecorder struct {
	mu        sync.Mutex
	domains   []int
	fallbacks []string
}

func (*mdRecorder) Result(string)     {}
func (*mdRecorder) Reply(int, string) {}
func (*mdRecorder) Blocked(string)    {}
func (m *mdRecorder) SessionDomains(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.domains = append(m.domains, n)
}

func (m *mdRecorder) MultiDomainFallback(family, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallbacks = append(m.fallbacks, family+"/"+reason)
}

type fallback struct{ family, host, reason, reply string }

type rig struct {
	p         *prober.Prober
	pacer     *countingPacer
	rec       *mdRecorder
	fallbacks []fallback
	refusals  []string // replies the stand-down hook saw
}

// newRig builds a prober against port. multi says whether the family is
// cleared for grouping; the family is always "@google".
func newRig(port string, multi bool, extra ...func(*prober.Options)) *rig {
	r := &rig{pacer: &countingPacer{}, rec: &mdRecorder{}}
	opts := prober.Options{
		Resolver:    loopbackResolver{},
		Helo:        "mail.datascoutmail.com",
		MailFrom:    "verify@probe.datascoutmail.com",
		Port:        port,
		DialNetwork: "tcp4",
		Timeout:     10 * time.Second,
		Pacer:       r.pacer,
		Metrics:     r.rec,
		MultiDomain: func(string) (string, bool) { return "@google", multi },
		OnFallback: func(family, host, reason, reply string) {
			r.fallbacks = append(r.fallbacks, fallback{family, host, reason, reply})
		},
		OnRefusal: func(_, reply string) { r.refusals = append(r.refusals, reply) },
	}
	for _, f := range extra {
		f(&opts)
	}
	r.p = prober.New(opts)
	return r
}

func threeDomains() prober.Request {
	return prober.Request{
		MXHost: "aspmx.l.google.com",
		Domains: []prober.DomainGroup{
			{Domain: "ws-a.test", Emails: []string{"valid@ws-a.test", "nope@ws-a.test"}, NeedCatchAll: true},
			{Domain: "ws-b.test", Emails: []string{"ceo@ws-b.test", "ghost@ws-b.test"}, NeedCatchAll: true},
			{Domain: "ws-c.test", Emails: []string{"anyone@ws-c.test"}, NeedCatchAll: true},
		},
	}
}

func probe(t *testing.T, p *prober.Prober, req prober.Request) prober.Response {
	t.Helper()
	resp, err := p.Probe(t.Context(), req)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return resp
}

// verdicts strips a response to what a caller decides on.
func verdicts(resp prober.Response) map[string][3]any {
	out := map[string][3]any{}
	deref := func(b *bool) any {
		if b == nil {
			return nil
		}
		return *b
	}
	for a, r := range resp.Results {
		out[a] = [3]any{r.Class, deref(r.Accepted), deref(r.CatchAll)}
	}
	return out
}

// The plan-033 shape: one session, three domains, each domain's answers and
// its own catch-all verdict right.
func TestThreeWorkspaceDomainsShareOneSession(t *testing.T) {
	port, eng := startWorkspace(t)
	r := newRig(port, true)
	resp := probe(t, r.p, threeDomains())

	want := map[string][3]any{
		"valid@ws-a.test":  {prober.ClassValid, true, false},
		"nope@ws-a.test":   {prober.ClassInvalid, false, false},
		"ceo@ws-b.test":    {prober.ClassValid, true, false},
		"ghost@ws-b.test":  {prober.ClassInvalid, false, false},
		"anyone@ws-c.test": {prober.ClassValid, true, true},
	}
	if got := verdicts(resp); !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts\n got %v\nwant %v", got, want)
	}
	if st := eng.Stats(); st.Conns != 1 {
		t.Errorf("server saw %d connections, want 1", st.Conns)
	}
	if r.pacer.leases != 1 {
		t.Errorf("leases = %d, want one per session", r.pacer.leases)
	}
	// One token per RCPT across every domain, real and catch-all alike.
	if st := eng.Stats(); int64(len(r.pacer.tokens)) != st.Rcpt {
		t.Errorf("tokens %d for %d RCPTs the server saw", len(r.pacer.tokens), st.Rcpt)
	}
	for _, h := range r.pacer.tokens {
		if h != "aspmx.l.google.com" {
			t.Errorf("token drawn for %q, want the request's MX (the pacer keys it to @google)", h)
		}
	}
	if !reflect.DeepEqual(r.rec.domains, []int{3}) {
		t.Errorf("session_domains observed %v, want [3]", r.rec.domains)
	}
	if st := eng.Stats(); st.DomainLimited != 0 || len(r.fallbacks) != 0 {
		t.Errorf("domain-limit refusals %d, fallbacks %+v — want none", st.DomainLimited, r.fallbacks)
	}
}

// The production failure of 2026-10-02 (Data Scout job 766, flag on): 83
// Google Workspace domains went into 31 sessions with every domain in one
// transaction, and Google answered each RCPT for a second domain "451-4.3.0
// Multiple destination domains per transaction is unsupported. Please try
// again." — 52 of 83 addresses came back unknown, and the relay fallback never
// fired because a 4xx deferral is not a relay refusal. The same shape against
// a server enforcing Google's rule: every address answered, in one connection,
// and never once the refusal.
func TestGoogleDomainPerTransactionRuleIsHonoured(t *testing.T) {
	port, eng := startWorkspace(t, func(p *policy.Profile) {
		p.Domains = nil // every domain is Google's, as Workspace's are
		p.Behaviour.CatchAllDomains = nil
	})
	r := newRig(port, true)
	req := prober.Request{MXHost: "aspmx.l.google.com"}
	want := map[string][3]any{}
	for i := range 6 {
		d := fmt.Sprintf("ws-%d.test", i)
		req.Domains = append(req.Domains, prober.DomainGroup{
			Domain: d, Emails: []string{"valid@" + d, "nope@" + d}, NeedCatchAll: true,
		})
		want["valid@"+d] = [3]any{prober.ClassValid, true, false}
		want["nope@"+d] = [3]any{prober.ClassInvalid, false, false}
	}
	resp := probe(t, r.p, req)
	if got := verdicts(resp); !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts\n got %v\nwant %v", got, want)
	}
	st := eng.Stats()
	if st.DomainLimited != 0 {
		t.Errorf("Google refused %d RCPTs as a second domain in a transaction, want 0", st.DomainLimited)
	}
	if st.Conns != 1 || r.pacer.leases != 1 || !reflect.DeepEqual(r.rec.domains, []int{6}) {
		t.Errorf("connections %d, leases %d, session_domains %v — want 1, 1, [6]", st.Conns, r.pacer.leases, r.rec.domains)
	}
	if len(r.fallbacks) != 0 {
		t.Errorf("fallbacks %+v, want none", r.fallbacks)
	}
	if int64(len(r.pacer.tokens)) != st.Rcpt {
		t.Errorf("tokens %d for %d RCPTs the server saw", len(r.pacer.tokens), st.Rcpt)
	}
}

// A system that takes one transaction per connection refuses the second MAIL
// FROM: grouping falls back for the family (reason mail_from) and the rest are
// answered one domain per session inside the same request — no address lost.
func TestASecondTransactionRefusedFallsBackAndAnswersEverything(t *testing.T) {
	port, eng := startProfile(t, "one-transaction")
	r := newRig(port, true)
	resp := probe(t, r.p, threeDomains())

	want := map[string][3]any{
		"valid@ws-a.test":  {prober.ClassValid, true, false},
		"nope@ws-a.test":   {prober.ClassInvalid, false, false},
		"ceo@ws-b.test":    {prober.ClassValid, true, false},
		"ghost@ws-b.test":  {prober.ClassInvalid, false, false},
		"anyone@ws-c.test": {prober.ClassValid, true, true},
	}
	if got := verdicts(resp); !reflect.DeepEqual(got, want) {
		t.Errorf("verdicts\n got %v\nwant %v", got, want)
	}
	if len(r.fallbacks) != 1 || r.fallbacks[0].reason != prober.FallbackMailFrom ||
		!reflect.DeepEqual(r.rec.fallbacks, []string{"@google/mail_from"}) {
		t.Fatalf("fallbacks %+v / counted %v, want one @google mail_from", r.fallbacks, r.rec.fallbacks)
	}
	// Refusing a second transaction is not a refusal of our IP.
	if len(r.refusals) != 0 {
		t.Errorf("the stand-down saw %v", r.refusals)
	}
	// The grouped session (ws-a answered), then ws-b and ws-c alone.
	if st := eng.Stats(); st.Conns != 3 || st.MailRefused != 1 {
		t.Errorf("connections %d, refused MAIL FROMs %d — want 3 and 1", st.Conns, st.MailRefused)
	}
	if int64(len(r.pacer.tokens)) != eng.Stats().Rcpt {
		t.Errorf("tokens %d for %d RCPTs the server saw", len(r.pacer.tokens), eng.Stats().Rcpt)
	}
}

// A family without multi_domain gets one session per domain — the same answers.
func TestAFamilyWithoutMultiDomainGetsOneSessionPerDomain(t *testing.T) {
	port, eng := startWorkspace(t)
	grouped := verdicts(probe(t, newRig(port, true).p, threeDomains()))
	before := eng.Stats().Conns

	r := newRig(port, false)
	alone := verdicts(probe(t, r.p, threeDomains()))
	if !reflect.DeepEqual(alone, grouped) {
		t.Errorf("one session per domain answered differently\n alone %v\ngrouped %v", alone, grouped)
	}
	if got := eng.Stats().Conns - before; got != 3 {
		t.Errorf("connections = %d, want 3 (one per domain)", got)
	}
	if r.pacer.leases != 3 || !reflect.DeepEqual(r.rec.domains, []int{1, 1, 1}) {
		t.Errorf("leases %d, session_domains %v — want 3 and [1 1 1]", r.pacer.leases, r.rec.domains)
	}
}

// Whole domains are packed up to the per-session cap; a session never exceeds it.
func TestGroupsPackWholeDomainsUpToTheCap(t *testing.T) {
	port, eng := startWorkspace(t)
	r := newRig(port, true, func(o *prober.Options) { o.MaxRCPTPerSession = 3 })
	req := threeDomains()
	for i := range req.Domains {
		req.Domains[i].NeedCatchAll = false
	}
	probe(t, r.p, req)
	// 2 + 2 > 3, so ws-a alone; ws-b (2) + ws-c (1) together.
	if got := eng.Stats().Conns; got != 2 || !reflect.DeepEqual(r.rec.domains, []int{1, 2}) {
		t.Errorf("connections %d, session_domains %v — want 2 and [1 2]", got, r.rec.domains)
	}
}

// A receiver that answers for its own domains only: the foreign domain's
// address is policy — never invalid — grouping stops for the family on this
// node, and what the session had not asked yet is asked again alone.
func TestARelayRefusalIsPolicyAndEndsGrouping(t *testing.T) {
	port, eng := startWorkspace(t, func(p *policy.Profile) {
		p.Domains = []string{"ws-a.test", "ws-c.test"}
		// No enhanced code: the classifier alone would call a 550 invalid.
		p.Behaviour.RelayDenied = "550 <%s>: relay not permitted"
		p.Behaviour.CatchAllDomains = nil
	})
	r := newRig(port, true)
	req := prober.Request{
		MXHost: "aspmx.l.google.com",
		Domains: []prober.DomainGroup{
			{Domain: "ws-a.test", Emails: []string{"valid@ws-a.test", "nope@ws-a.test"}, NeedCatchAll: true},
			{Domain: "ws-b.test", Emails: []string{"valid@ws-b.test"}, NeedCatchAll: true},
			{Domain: "ws-c.test", Emails: []string{"valid@ws-c.test", "nope@ws-c.test"}, NeedCatchAll: true},
		},
	}
	resp := probe(t, r.p, req)

	refused := resp.Results["valid@ws-b.test"]
	if refused.Class != prober.ClassPolicy || refused.Accepted != nil {
		t.Errorf("relay refusal = %s accepted %v, want policy with no verdict", refused.Class, refused.Accepted)
	}
	for addr, want := range map[string]prober.Class{
		"valid@ws-a.test": prober.ClassValid, "nope@ws-a.test": prober.ClassInvalid,
		"valid@ws-c.test": prober.ClassValid, "nope@ws-c.test": prober.ClassInvalid,
	} {
		if got := resp.Results[addr]; got.Class != want || got.CatchAll == nil || *got.CatchAll {
			t.Errorf("%s = %s catch_all %v, want %s and an honest server", addr, got.Class, got.CatchAll, want)
		}
	}
	if len(r.fallbacks) != 1 || r.fallbacks[0].family != "@google" || r.fallbacks[0].reason != prober.FallbackRelay ||
		!reflect.DeepEqual(r.rec.fallbacks, []string{"@google/relay"}) {
		t.Fatalf("fallbacks %+v / counted %v, want one for @google", r.fallbacks, r.rec.fallbacks)
	}
	if r.fallbacks[0].reply == "" || r.fallbacks[0].reply == "550 <valid@ws-b.test>: relay not permitted" {
		t.Errorf("fallback reply %q, want it logged redacted", r.fallbacks[0].reply)
	}
	// A relay refusal is about the question, not our reputation.
	if len(r.refusals) != 0 {
		t.Errorf("the stand-down saw a relay refusal: %v", r.refusals)
	}
	// The grouped session, then ws-c alone.
	if got := eng.Stats().Conns; got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}
	// The receiver was asked about the foreign domain once.
	if got := eng.Stats().RelayDenied; got != 1 {
		t.Errorf("relay refusals = %d, want 1", got)
	}

	// From now on this family is one domain per session on this node.
	before := eng.Stats().Conns
	probe(t, r.p, prober.Request{MXHost: "aspmx.l.google.com", Domains: []prober.DomainGroup{
		{Domain: "ws-a.test", Emails: []string{"valid@ws-a.test"}},
		{Domain: "ws-c.test", Emails: []string{"valid@ws-c.test"}},
	}})
	if got := eng.Stats().Conns - before; got != 2 || len(r.fallbacks) != 1 {
		t.Errorf("after the fallback: %d connections, %d fallbacks — want 2 and still 1", got, len(r.fallbacks))
	}
}

// A refusal of our client is not per domain: the policy stop ends every
// domain's remaining RCPTs as unattempted.
func TestPolicyStopEndsEveryDomainInTheSession(t *testing.T) {
	port, eng := startWorkspace(t, func(p *policy.Profile) {
		p.Behaviour.RejectUnknown = "550 5.7.1 Client host rejected: Access denied by policy"
	})
	r := newRig(port, true, func(o *prober.Options) { o.PolicyStop = 2 })
	resp := probe(t, r.p, prober.Request{MXHost: "aspmx.l.google.com", Domains: []prober.DomainGroup{
		{Domain: "ws-a.test", Emails: []string{"nope@ws-a.test"}, NeedCatchAll: true},
		{Domain: "ws-b.test", Emails: []string{"ghost@ws-b.test", "valid@ws-b.test"}, NeedCatchAll: true},
		{Domain: "ws-c.test", Emails: []string{"valid@ws-c.test"}, NeedCatchAll: true},
	}})
	for _, a := range []string{"valid@ws-b.test", "valid@ws-c.test"} {
		if got := resp.Results[a]; got.Class != prober.ClassPolicy || got.Connected == nil || *got.Connected {
			t.Errorf("%s = %+v, want unattempted policy", a, got)
		}
	}
	if st := eng.Stats(); st.Rcpt != 2 || st.Conns != 1 {
		t.Errorf("server saw %d RCPTs over %d connections, want 2 over 1", st.Rcpt, st.Conns)
	}
	if len(r.fallbacks) != 0 || len(r.refusals) != 2 {
		t.Errorf("fallbacks %+v, refusals %d — want none and the two seen", r.fallbacks, len(r.refusals))
	}
}

// A randomising server condemns every domain on it (invariant 7), whichever
// domain revealed it.
type knownRandomiser struct{}

func (knownRandomiser) IsRandomiser(context.Context, string) bool { return true }
func (knownRandomiser) MarkRandomiser(context.Context, string)    {}

func TestAKnownRandomiserCondemnsEveryDomain(t *testing.T) {
	port, _ := startWorkspace(t)
	r := newRig(port, true, func(o *prober.Options) { o.Profiles = knownRandomiser{} })
	resp := probe(t, r.p, threeDomains())
	for a, res := range resp.Results {
		if res.Randomiser == nil || !*res.Randomiser || res.CatchAll == nil || !*res.CatchAll {
			t.Errorf("%s: randomiser %v catch_all %v, want both true", a, res.Randomiser, res.CatchAll)
		}
	}
}

// Two callers' batches for one domain meet in one request: merged, asked once.
func TestTheSameDomainTwiceIsAskedOnce(t *testing.T) {
	port, eng := startWorkspace(t)
	r := newRig(port, true)
	resp := probe(t, r.p, prober.Request{MXHost: "aspmx.l.google.com", Domains: []prober.DomainGroup{
		{Domain: "ws-a.test", Emails: []string{"valid@ws-a.test"}},
		{Domain: "WS-A.test", Emails: []string{"valid@ws-a.test", "nope@ws-a.test"}, NeedCatchAll: true},
	}})
	if len(resp.Results) != 2 || resp.Results["valid@ws-a.test"].CatchAll == nil {
		t.Errorf("results %v, want two with a catch-all verdict", verdicts(resp))
	}
	if st := eng.Stats(); st.Conns != 1 {
		t.Errorf("connections = %d, want 1", st.Conns)
	}
	if !reflect.DeepEqual(r.rec.domains, []int{1}) {
		t.Errorf("session_domains %v, want [1]", r.rec.domains)
	}
}

// Plan 034: every answer names the receiving system and whether it may be
// grouped right now — for both request shapes.
func TestTheAnswerNamesTheSystemAndWhetherItGroups(t *testing.T) {
	port, _ := startWorkspace(t)
	for _, multi := range []bool{true, false} {
		r := newRig(port, multi)
		grouped := probe(t, r.p, threeDomains())
		single := probe(t, r.p, prober.Request{
			MXHost: "aspmx.l.google.com", Domain: "ws-a.test",
			Emails: []string{"valid@ws-a.test"},
		})
		for name, resp := range map[string]prober.Response{"domains": grouped, "single": single} {
			if resp.PaceKey != "@google" || resp.MultiDomain != multi {
				t.Errorf("multi=%v %s: pace_key %q multi_domain %v, want @google %v",
					multi, name, resp.PaceKey, resp.MultiDomain, multi)
			}
		}
	}
}

// The request that causes a fallback already answers multi_domain=false, so
// the caller forms no further cross-host group on stale information.
func TestTheRequestThatFallsBackAlreadySaysSo(t *testing.T) {
	port, _ := startProfile(t, "one-transaction")
	r := newRig(port, true)
	resp := probe(t, r.p, threeDomains())
	if len(r.fallbacks) != 1 {
		t.Fatalf("fallbacks %+v, want one", r.fallbacks)
	}
	if resp.PaceKey != "@google" || resp.MultiDomain {
		t.Errorf("pace_key %q multi_domain %v after the fallback, want @google false",
			resp.PaceKey, resp.MultiDomain)
	}
}

// Without a family function (tests, a stripped build) the fields stay empty.
func TestNoFamilyFunctionNamesNoSystem(t *testing.T) {
	port, _ := startWorkspace(t)
	r := newRig(port, true, func(o *prober.Options) { o.MultiDomain = nil })
	resp := probe(t, r.p, prober.Request{
		MXHost: "aspmx.l.google.com", Domain: "ws-a.test", Emails: []string{"valid@ws-a.test"},
	})
	if resp.PaceKey != "" || resp.MultiDomain {
		t.Errorf("pace_key %q multi_domain %v, want empty and false", resp.PaceKey, resp.MultiDomain)
	}
}
