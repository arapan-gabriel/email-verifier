package prober

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
)

// Plan 027, Design 3: one table of scripted sessions covering every branch of
// the SMTP state machine. Each row is a server transcript plus fakes, and
// asserts the per-address classes, what the pacer was told, how many tokens
// were taken, and what IP health heard.

// transcriptMX answers from a map of command prefix → reply, and can close the
// connection at a named point. Unlike scriptedMX it can also say nothing at
// all, for EOF and tarpit rows.
type transcriptMX struct {
	banner   string // "" closes immediately; "-" stays silent (tarpit)
	replies  []step // matched in order of prefix, first match wins
	closeOn  string // close the connection when a command with this prefix arrives…
	skip     int    // …after letting this many such commands through
	mu       sync.Mutex
	commands []string
}

type step struct{ prefix, reply string }

func (m *transcriptMX) DialContext(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		switch m.banner {
		case "":
			return
		case "-":
			_, _ = bufio.NewReader(server).ReadString('\n') // never answers
			return
		}
		if _, err := fmt.Fprintf(server, "%s\r\n", m.banner); err != nil {
			return
		}
		br := bufio.NewReader(server)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimRight(line, "\r\n")
			m.mu.Lock()
			m.commands = append(m.commands, cmd)
			m.mu.Unlock()
			if m.closeOn != "" && strings.HasPrefix(cmd, m.closeOn) {
				if m.skip == 0 {
					return
				}
				m.skip--
			}
			if strings.HasPrefix(cmd, "QUIT") {
				return
			}
			if strings.HasPrefix(cmd, "RSET") {
				continue
			}
			for _, s := range m.replies {
				if strings.HasPrefix(cmd, s.prefix) {
					if s.reply != "" {
						if _, err := fmt.Fprintf(server, "%s\r\n", s.reply); err != nil {
							return
						}
					}
					break
				}
			}
		}
	}()
	return client, nil
}

func ok250() []step {
	return []step{{"EHLO", "250 mx.test"}, {"MAIL FROM", "250 2.1.0 ok"}, {"RCPT TO", "250 2.1.5 ok"}}
}

// budgetPacer allows the first n acquires, then fails with err.
type budgetPacer struct {
	mu       sync.Mutex
	n        int
	err      error
	acquires int
	signals  []bool
}

func (b *budgetPacer) Acquire(context.Context, string, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acquires++
	if b.n >= 0 && b.acquires > b.n {
		return b.err
	}
	return nil
}

func (b *budgetPacer) Observe(_ context.Context, _ string, throttled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signals = append(b.signals, throttled)
}

type healthFake struct {
	burned   bool
	mu       sync.Mutex
	policies int
}

func (h *healthFake) Burned() (bool, string) { return h.burned, "listed on zen.example" }
func (h *healthFake) ObservePolicy(string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policies++
}

type resolverFunc func(context.Context, string) ([]netip.Addr, error)

func (f resolverFunc) Resolve(ctx context.Context, h string) ([]netip.Addr, error) { return f(ctx, h) }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// countingMetrics records what record() reported.
type countingMetrics struct {
	mu      sync.Mutex
	results map[string]int
	replies map[string]int
	blocked map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{results: map[string]int{}, replies: map[string]int{}, blocked: map[string]int{}}
}

func (c *countingMetrics) Result(class string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[class]++
}

func (c *countingMetrics) Reply(code int, class string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies[fmt.Sprintf("%d/%s", code, class)]++
}

func (c *countingMetrics) Blocked(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked[reason]++
}

func TestSessionTable(t *testing.T) {
	two := []string{"a@example.test", "b@example.test"}
	three := []string{"a@example.test", "b@example.test", "c@example.test"}
	for _, tc := range []struct {
		name       string
		mx         *transcriptMX
		dialer     Dialer // overrides mx
		resolver   Resolver
		pacer      *budgetPacer
		health     *healthFake
		emails     []string
		catchAll   bool
		want       []Class // per address, in order
		throttles  int     // Observe(true) calls
		acquires   int     // -1: don't check
		policies   int     // IP health policy observations
		noAccepted bool    // every result must carry Accepted == nil
	}{
		{
			name: "ip burned: nothing resolved, nothing taken, nothing dialed", health: &healthFake{burned: true},
			mx: &transcriptMX{banner: "220 x", replies: ok250()}, emails: two,
			want: []Class{ClassIPBurned, ClassIPBurned}, noAccepted: true,
		},
		{
			name: "resolve times out", resolver: resolverFunc(func(context.Context, string) ([]netip.Addr, error) { return nil, timeoutErr{} }),
			emails: two, want: []Class{ClassTimeout, ClassTimeout}, noAccepted: true,
		},
		{
			name: "resolve fails otherwise", resolver: resolverFunc(func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("SERVFAIL") }),
			emails: two, want: []Class{ClassConnError, ClassConnError}, noAccepted: true,
		},
		{
			name: "guard refusal", resolver: resolverFunc(func(context.Context, string) ([]netip.Addr, error) { return nil, resolver.ErrNoRoutableAddress }),
			emails: two, want: []Class{ClassGuarded, ClassGuarded}, noAccepted: true,
		},
		{
			name: "no address to dial", resolver: resolverFunc(func(context.Context, string) ([]netip.Addr, error) { return nil, nil }),
			emails: two, want: []Class{ClassConnError, ClassConnError}, acquires: 1, noAccepted: true,
		},
		{
			name: "every IP refuses the dial", resolver: twoIPs(),
			dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }),
			emails: two, want: []Class{ClassConnError, ClassConnError}, acquires: 1, noAccepted: true,
		},
		{
			name: "banner EOF", mx: &transcriptMX{banner: ""}, emails: two,
			want: []Class{ClassConnError, ClassConnError}, acquires: 1, noAccepted: true,
		},
		{
			name: "banner 421 is throttling", mx: &transcriptMX{banner: "421 4.7.0 too many connections"}, emails: two,
			want: []Class{ClassThrottled, ClassThrottled}, throttles: 1, acquires: 1, noAccepted: true,
		},
		{
			name: "banner 554 is about us", mx: &transcriptMX{banner: "554 5.7.1 go away"}, emails: two,
			want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		{
			name: "EHLO dies", mx: &transcriptMX{banner: "220 x", closeOn: "EHLO"}, emails: two,
			want: []Class{ClassConnError, ClassConnError}, acquires: 1, noAccepted: true,
		},
		{
			name: "EHLO 5xx", mx: &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "550 5.7.1 helo command rejected"}}}, emails: two,
			want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		{
			name: "MAIL FROM dies", mx: &transcriptMX{banner: "220 x", replies: ok250(), closeOn: "MAIL"}, emails: two,
			want: []Class{ClassConnError, ClassConnError}, acquires: 1, noAccepted: true,
		},
		{
			name: "MAIL FROM 5xx", mx: &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"}, {"MAIL FROM", "550 5.7.1 your mail from is refused"}}}, emails: two,
			want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		// Invariant 1, found by this table: a 5xx before MAIL FROM succeeds is
		// a rejection of us whatever its wording, and must never class invalid
		// — Data Scout derives `accepted` from `class`, so an invalid here
		// condemns every address in the chunk. Postfix's
		// reject_unlisted_sender answers exactly this way.
		{
			name: "MAIL FROM refused with mailbox wording is still about us",
			mx: &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"},
				{"MAIL FROM", "550 5.1.0 <verify@probe.test>: Sender address rejected: User unknown in virtual mailbox table"}}},
			emails: two, want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		{
			name: "banner 5xx with mailbox wording is still about us", mx: &transcriptMX{banner: "550 no such user here"}, emails: two,
			want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		{
			name: "EHLO 5xx with mailbox wording is still about us",
			mx:   &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "550 5.1.1 user unknown"}}}, emails: two,
			want: []Class{ClassPolicy, ClassPolicy}, acquires: 1, noAccepted: true,
		},
		{
			name: "a banner that is 2xx but not 220 is not a verdict", mx: &transcriptMX{banner: "250 hello"}, emails: two,
			want: []Class{ClassUnknown, ClassUnknown}, acquires: 1, noAccepted: true,
		},
		{
			name: "an EHLO answered 2xx-but-not-250 is not a verdict",
			mx:   &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "220 again"}}}, emails: two,
			want: []Class{ClassUnknown, ClassUnknown}, acquires: 1, noAccepted: true,
		},
		{
			name: "MAIL FROM 4xx defers everyone", mx: &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"}, {"MAIL FROM", "451 4.3.0 try later"}}}, emails: two,
			want: []Class{ClassDeferred, ClassDeferred}, acquires: 1, noAccepted: true,
		},
		{
			name: "budget runs out mid-batch: answered keep answers, rest unattempted",
			mx:   &transcriptMX{banner: "220 x", replies: ok250()}, pacer: &budgetPacer{n: 2, err: errors.New("redis down")},
			emails: three, want: []Class{ClassValid, ClassValid, ClassNoBudget}, acquires: 3,
		},
		{
			name: "pause mid-batch is paused, not no_budget",
			mx:   &transcriptMX{banner: "220 x", replies: ok250()}, pacer: &budgetPacer{n: 1, err: &pacer.PausedError{MXHost: "mx.test"}},
			emails: three, want: []Class{ClassValid, ClassPaused, ClassPaused}, acquires: 2,
		},
		{
			name: "connection dies mid-batch", mx: &transcriptMX{banner: "220 x", replies: ok250(), closeOn: "RCPT TO:<b@"},
			emails: three, want: []Class{ClassValid, ClassConnError, ClassConnError}, acquires: 2,
		},
		{
			name: "a policy reply feeds IP health, never the pacer",
			mx: &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"}, {"MAIL FROM", "250 ok"},
				{"RCPT TO:<a@", "550 5.7.1 client host blocked using spamhaus"}, {"RCPT TO", "250 ok"}}},
			health: &healthFake{}, emails: two, want: []Class{ClassPolicy, ClassValid}, policies: 1, acquires: 2,
		},
		{
			name: "catch-all loop stops on budget", mx: &transcriptMX{banner: "220 x", replies: ok250()},
			pacer: &budgetPacer{n: 2, err: errors.New("redis down")}, emails: two, catchAll: true,
			want: []Class{ClassValid, ClassValid}, acquires: 3,
		},
		{
			name: "catch-all loop stops when the connection dies", mx: &transcriptMX{banner: "220 x", replies: []step{
				{"EHLO", "250 x"}, {"MAIL FROM", "250 ok"}, {"RCPT TO", "250 ok"}}, closeOn: "RCPT TO", skip: 2},
			emails: []string{"a@example.test"}, catchAll: true, want: []Class{ClassValid}, acquires: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := tc.pacer
			if pc == nil {
				pc = &budgetPacer{n: -1}
			}
			res := tc.resolver
			if res == nil {
				res = stubResolver{}
			}
			d := tc.dialer
			dials := 0
			if d == nil && tc.mx != nil {
				d = tc.mx
			}
			if d == nil {
				d = dialFunc(func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("must not dial")
				})
			}
			counting := dialFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
				dials++
				return d.DialContext(ctx, n, a)
			})
			opts := Options{Dialer: counting, Resolver: res, Pacer: pc, Timeout: 5 * time.Second, CatchAllProbes: 3}
			if tc.health != nil {
				opts.Health = tc.health
			}
			m := newCountingMetrics()
			opts.Metrics = m
			resp, err := New(opts).Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: tc.emails, NeedCatchAll: tc.catchAll})
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			for i, addr := range tc.emails {
				r := resp.Results[addr]
				if r.Class != tc.want[i] {
					t.Errorf("%s: class %s, want %s (reply %q err %q)", addr, r.Class, tc.want[i], r.Reply, r.Err)
				}
				if tc.noAccepted && r.Accepted != nil {
					t.Errorf("%s: Accepted = %v on a non-answer", addr, *r.Accepted)
				}
				if r.Class == ClassInvalid && r.Accepted == nil {
					t.Errorf("%s: class invalid with no RCPT answer (invariant 1)", addr)
				}
			}
			n := 0
			for _, s := range pc.signals {
				if s {
					n++
				}
			}
			if n != tc.throttles {
				t.Errorf("pacer saw %d throttles, want %d (%v)", n, tc.throttles, pc.signals)
			}
			if tc.acquires >= 0 && pc.acquires != tc.acquires {
				t.Errorf("took %d tokens, want %d", pc.acquires, tc.acquires)
			}
			if tc.health != nil && tc.health.policies != tc.policies {
				t.Errorf("IP health heard %d policy replies, want %d", tc.health.policies, tc.policies)
			}
			if tc.health != nil && tc.health.burned && dials != 0 {
				t.Errorf("a burned node dialed %d times", dials)
			}
			total := 0
			for _, v := range m.results {
				total += v
			}
			if total != len(tc.emails) {
				t.Errorf("record() counted %d results for %d addresses", total, len(tc.emails))
			}
		})
	}
}

func twoIPs() Resolver {
	return resolverFunc(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.51.100.10"), netip.MustParseAddr("198.51.100.11")}, nil
	})
}

// The first vetted address refusing is not the end: the second is tried, as a
// real sender does.
func TestDialFallsThroughToTheNextAddress(t *testing.T) {
	var targets []string
	mx := &transcriptMX{banner: "220 x", replies: ok250()}
	p := New(Options{
		Resolver: twoIPs(), Timeout: 5 * time.Second,
		Dialer: dialFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
			targets = append(targets, a)
			if len(targets) == 1 {
				return nil, errors.New("connection refused")
			}
			return mx.DialContext(ctx, n, a)
		}),
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results["a@example.test"].Class != ClassValid || len(targets) != 2 || targets[1] != "198.51.100.11:25" {
		t.Errorf("result %+v after dialing %v", resp.Results["a@example.test"], targets)
	}
}

// A server that accepts the connection and never speaks is held only as long
// as the session timeout, and the answer is timeout — never a verdict. Under
// synctest the 20-second default elapses instantly.
func TestTarpitBannerTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mx := &transcriptMX{banner: "-"}
		p := New(Options{Dialer: mx, Resolver: stubResolver{}}) // Timeout 0 → the 20s default
		start := time.Now()
		resp, err := p.Probe(context.Background(), Request{MXHost: "mx.test", Emails: []string{"a@example.test"}})
		if err != nil {
			t.Fatal(err)
		}
		r := resp.Results["a@example.test"]
		if r.Class != ClassTimeout || r.Accepted != nil {
			t.Errorf("tarpit = %+v, want timeout with no verdict", r)
		}
		if waited := time.Since(start); waited != 20*time.Second {
			t.Errorf("held for %s, want the 20s default session timeout", waited)
		}
	})
}

// record() reports every class once, keeps our own refusals apart as
// "blocked", and counts a policy stop as its own block reason.
func TestRecordCountsBlockedApart(t *testing.T) {
	m := newCountingMetrics()
	p := New(Options{Metrics: m})
	for _, r := range []Result{
		{Class: ClassValid, SMTPCode: 250}, {Class: ClassGuarded}, {Class: ClassNoBudget}, {Class: ClassPaused},
		{Class: ClassIPBurned}, {Class: ClassSuppressed}, {Class: ClassPolicy, SMTPCode: 550},
		{Class: ClassPolicy, Err: "not attempted: 5 consecutive policy replies from this server"},
	} {
		p.record("mx.test", r)
	}
	for _, reason := range []string{"guarded", "no_budget", "paused", "ip_burned", "suppressed", "policy_stop"} {
		if m.blocked[reason] != 1 {
			t.Errorf("blocked[%s] = %d, want 1", reason, m.blocked[reason])
		}
	}
	if m.blocked["policy"] != 0 {
		t.Error("an answered policy reply is not a block")
	}
	if m.results["policy"] != 2 || m.replies["250/valid"] != 1 {
		t.Errorf("results %v replies %v", m.results, m.replies)
	}
}

// Concurrent Probe calls share the randomiser memory without a race.
func TestConcurrentProbesShareProfiles(t *testing.T) {
	prof := newProfiles()
	p := New(Options{Dialer: coinFlipMX(), Resolver: stubResolver{}, Profiles: prof, Timeout: 5 * time.Second, CatchAllProbes: 4})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Probe(context.Background(), Request{
				MXHost: "mx.test", Domain: fmt.Sprintf("d%d.test", i), Emails: []string{"a@x.test"}, NeedCatchAll: true,
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// An out-of-range code arriving at RCPT is a broken session, never a verdict
// (fix 1, end to end).
func TestOutOfRangeCodeAtRCPTIsNotAVerdict(t *testing.T) {
	for _, reply := range []string{"999 no such user", "600 user unknown"} {
		mx := &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"}, {"MAIL FROM", "250 ok"}, {"RCPT TO", reply}}}
		resp := probeWith(t, mx, Request{MXHost: "mx.test", Emails: []string{"a@example.test"}})
		r := resp.Results["a@example.test"]
		if r.Class == ClassInvalid || r.Accepted != nil {
			t.Errorf("RCPT answered %q gave %+v, want a non-answer", reply, r)
		}
	}
}

// The retry hint end to end, through Probe (plan 027): an explicit server hint
// in each unit, clamped both ways; no hint means the configured deferral
// retry (or its 15-minute default); answers carry no hint at all.
func TestRetryHintThroughProbe(t *testing.T) {
	for _, tc := range []struct {
		reply    string
		deferral time.Duration
		want     int
	}{
		{"450 4.2.0 greylisted, try again in 90 seconds", 0, 90},
		{"450 4.2.0 greylisted, try again in 5 minutes", 0, 300},
		{"450 4.2.0 greylisted, retry in 2 hours", 0, 7200},
		{"450 4.2.0 greylisted, try again in 5 s", 0, 60},         // clamped up
		{"450 4.2.0 greylisted, try again in 99 hours", 0, 21600}, // clamped down
		{"450 4.2.0 greylisted", 0, 900},                          // the 15-minute default
		{"450 4.2.0 greylisted", 10 * time.Minute, 600},           // the configured one
		{"421 4.7.0 too many, try again in 2 minutes", 0, 120},    // throttled carries one too
		{"250 2.1.5 ok", 0, 0},
		{"550 5.1.1 no such user, try again in 5 minutes", 0, 0},
	} {
		mx := &transcriptMX{banner: "220 x", replies: []step{{"EHLO", "250 x"}, {"MAIL FROM", "250 ok"}, {"RCPT TO", tc.reply}}}
		p := New(Options{Dialer: mx, Resolver: stubResolver{}, Timeout: 5 * time.Second, DeferralRetry: tc.deferral})
		resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Emails: []string{"a@example.test"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Results["a@example.test"].RetryAfterSeconds; got != tc.want {
			t.Errorf("%q (deferral %s): retry_after %d, want %d", tc.reply, tc.deferral, got, tc.want)
		}
	}
}

// A paused MX's hint is the exact remaining cooldown, not an estimate.
func TestPausedHintIsExact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		until := time.Now().Add(7*time.Minute + 30*time.Second)
		pc := &budgetPacer{n: 0, err: &pacer.PausedError{MXHost: "mx.test", Until: until}}
		p := New(Options{Pacer: pc, Resolver: stubResolver{}, Dialer: &transcriptMX{banner: "220 x"}})
		resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Emails: []string{"a@example.test", "b@example.test"}})
		if err != nil {
			t.Fatal(err)
		}
		for addr, r := range resp.Results {
			if r.Class != ClassPaused || r.RetryAfterSeconds != 450 {
				t.Errorf("%s: %s with hint %d, want paused with exactly 450", addr, r.Class, r.RetryAfterSeconds)
			}
		}
	})
}
