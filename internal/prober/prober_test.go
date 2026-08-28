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
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
)

// scriptedMX is a Dialer backed by an in-memory pipe, so no unit test opens a
// socket (ENGINEERING-STANDARDS §7). reply returns the line to send for a
// command, or "" to send nothing — the prober does not read the answers to
// RSET and QUIT, and an unread write on a pipe would block.
func scriptedMX(banner string, reply func(cmd string) string) Dialer {
	return dialFunc(func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			br := bufio.NewReader(server)
			if _, err := fmt.Fprintf(server, "%s\r\n", banner); err != nil {
				return
			}
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				cmd := strings.TrimRight(line, "\r\n")
				// The prober does not read the answers to RSET and QUIT, and an
				// unread write on a pipe blocks. A real socket buffers them.
				if strings.HasPrefix(cmd, "QUIT") || strings.HasPrefix(cmd, "RSET") {
					if strings.HasPrefix(cmd, "QUIT") {
						return
					}
					continue
				}
				if out := reply(cmd); out != "" {
					if _, err := fmt.Fprintf(server, "%s\r\n", out); err != nil {
						return
					}
				}
			}
		}()
		return client, nil
	})
}

// stubResolver hands back one routable address without touching DNS. Tests of
// the session must not depend on name resolution; the guard itself is tested
// separately, including through the prober's safe default.
type stubResolver struct{}

func (stubResolver) Resolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("198.51.100.10")}, nil
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, n, a string) (net.Conn, error) {
	return f(ctx, n, a)
}

// happyPath answers 250 to everything except the addresses named in bounce.
func happyPath(bounce ...string) func(string) string {
	return func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			return "250 mx.test greets you"
		case strings.HasPrefix(cmd, "MAIL FROM"):
			return "250 2.1.0 Ok"
		case strings.HasPrefix(cmd, "RCPT TO"):
			for _, b := range bounce {
				if strings.Contains(cmd, b) {
					return "550 5.1.1 The email account that you tried to reach does not exist"
				}
			}
			return "250 2.1.5 OK"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	}
}

func probeWith(t *testing.T, d Dialer, req Request) Response {
	t.Helper()
	p := New(Options{
		Dialer: d, Resolver: stubResolver{}, Timeout: 5 * time.Second,
		Helo: "mail.test", MailFrom: "verify@probe.test",
	})
	resp, err := p.Probe(t.Context(), req)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return resp
}

func TestProbeSeparatesRealFromMissing(t *testing.T) {
	d := scriptedMX("220 mx.test ESMTP", happyPath("ghost@"))
	resp := probeWith(t, d, Request{
		MXHost: "mx.test", Domain: "example.test",
		Emails: []string{"real@example.test", "ghost@example.test"},
	})

	live, ghost := resp.Results["real@example.test"], resp.Results["ghost@example.test"]
	if live.Accepted == nil || !*live.Accepted {
		t.Errorf("real address: Accepted = %v, want true", live.Accepted)
	}
	if live.Class != ClassValid {
		t.Errorf("real address: Class = %s, want valid", live.Class)
	}
	if ghost.Accepted == nil || *ghost.Accepted {
		t.Errorf("missing address: Accepted = %v, want false", ghost.Accepted)
	}
	if ghost.EnhancedCode != "5.1.1" {
		t.Errorf("missing address: EnhancedCode = %q, want 5.1.1", ghost.EnhancedCode)
	}
}

// The core of invariant 1: everything that fails before a good MAIL FROM is a
// rejection of us, and must never come back as "this mailbox does not exist".
func TestRejectionOfUsIsNeverARejectionOfTheAddress(t *testing.T) {
	for name, d := range map[string]Dialer{
		"421 in the banner": scriptedMX("421 4.7.0 Our system has detected an unusual rate of traffic from your IP", happyPath()),
		"554 in the banner": scriptedMX("554 5.7.1 Client host blocked using Spamhaus", happyPath()),
		"5xx at EHLO": scriptedMX("220 mx.test ESMTP", func(cmd string) string {
			if strings.HasPrefix(cmd, "EHLO") {
				return "550 5.7.25 Forward-confirmed reverse DNS failed"
			}
			return "250 ok"
		}),
		"blanket 550 at MAIL FROM": scriptedMX("220 mx.test ESMTP", func(cmd string) string {
			if strings.HasPrefix(cmd, "MAIL FROM") {
				return "550 5.7.1 Access denied"
			}
			return "250 ok"
		}),
		"connection refused": dialFunc(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connect: connection refused")
		}),
	} {
		t.Run(name, func(t *testing.T) {
			resp := probeWith(t, d, Request{
				MXHost: "mx.test", Domain: "example.test",
				Emails: []string{"a@example.test", "b@example.test"},
			})
			if len(resp.Results) != 2 {
				t.Fatalf("got %d results, want 2", len(resp.Results))
			}
			for addr, r := range resp.Results {
				if r.Accepted != nil {
					t.Errorf("%s: Accepted = %v, want nil — the question was never answered", addr, *r.Accepted)
				}
				if r.Connected == nil || *r.Connected {
					t.Errorf("%s: Connected = %v, want false", addr, r.Connected)
				}
				if r.Class == ClassInvalid {
					t.Errorf("%s: Class = invalid — a refusal of us was read as a missing mailbox", addr)
				}
			}
		})
	}
}

// fakeProfiles remembers randomiser verdicts in memory.
type fakeProfiles struct {
	mu     sync.Mutex
	marked map[string]bool
	reads  int
}

func newProfiles(known ...string) *fakeProfiles {
	f := &fakeProfiles{marked: map[string]bool{}}
	for _, h := range known {
		f.marked[h] = true
	}
	return f
}

func (f *fakeProfiles) IsRandomiser(_ context.Context, mxHost string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.marked[mxHost]
}

func (f *fakeProfiles) MarkRandomiser(_ context.Context, mxHost string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked[mxHost] = true
}

func (f *fakeProfiles) isMarked(mxHost string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked[mxHost]
}

// coinFlipMX accepts every other bogus recipient, which is what a
// Microsoft-class host looks like: one probe would report catch-all on one run
// and clean on the next.
func coinFlipMX() Dialer {
	var n int
	var mu sync.Mutex
	return scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO"):
			if strings.Contains(cmd, "real@") {
				return "250 2.1.5 OK"
			}
			mu.Lock()
			n++
			odd := n%2 == 1
			mu.Unlock()
			if odd {
				return "250 2.1.5 OK"
			}
			return "550 5.1.1 No such user"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	})
}

// A host answering by coin flip is a fact about the *server*: it condemns every
// domain behind it, including ones nobody has asked about yet.
func TestRandomiserIsDetectedAndRemembered(t *testing.T) {
	profiles := newProfiles()
	p := New(Options{
		Dialer: coinFlipMX(), Resolver: stubResolver{}, Profiles: profiles,
		CatchAllProbes: 4, Timeout: 5 * time.Second,
	})

	resp, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "first.test",
		Emails: []string{"real@first.test"}, NeedCatchAll: true,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	r := resp.Results["real@first.test"]
	if r.Randomiser == nil || !*r.Randomiser {
		t.Fatalf("Randomiser = %v, want true", r.Randomiser)
	}
	// Conservative reading, and the field callers already handle.
	if r.CatchAll == nil || !*r.CatchAll {
		t.Errorf("CatchAll = %v, want true for a randomiser", r.CatchAll)
	}
	if !profiles.isMarked("mx.test") {
		t.Error("the verdict was not remembered for the host")
	}
}

// The second request is for a different domain on the same host, and must carry
// the verdict without probing again.
func TestKnownRandomiserCondemnsOtherDomainsWithoutProbing(t *testing.T) {
	var bogus int
	var mu sync.Mutex
	d := scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO"):
			if !strings.Contains(cmd, "real@") {
				mu.Lock()
				bogus++
				mu.Unlock()
			}
			return "250 2.1.5 OK"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	})
	p := New(Options{
		Dialer: d, Resolver: stubResolver{}, Profiles: newProfiles("mx.test"),
		CatchAllProbes: 4, Timeout: 5 * time.Second,
	})

	resp, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "second.test",
		Emails: []string{"real@second.test"}, NeedCatchAll: true,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	r := resp.Results["real@second.test"]
	if r.Randomiser == nil || !*r.Randomiser {
		t.Errorf("a known randomiser did not condemn a neighbouring domain: %v", r.Randomiser)
	}
	mu.Lock()
	defer mu.Unlock()
	if bogus != 0 {
		t.Errorf("sent %d bogus probes to a host already known to randomise", bogus)
	}
}

// Asking three questions must cost three questions' worth of budget.
func TestCatchAllProbesCostBudget(t *testing.T) {
	pc := &recordingPacer{}
	p := New(Options{
		Dialer: scriptedMX("220 mx.test ESMTP", happyPath()), Resolver: stubResolver{},
		Pacer: pc, CatchAllProbes: 3, Timeout: 5 * time.Second,
	})
	if _, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test",
		Emails: []string{"a@example.test", "b@example.test"}, NeedCatchAll: true,
	}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := 2 + 3; pc.acquires != want {
		t.Errorf("took %d tokens for 2 recipients plus 3 catch-all probes, want %d", pc.acquires, want)
	}
}

func TestDecideCatchAll(t *testing.T) {
	for name, tc := range map[string]struct {
		accepted, answered   int
		catchAll, randomiser *bool
	}{
		"all accepted": {3, 3, ptrBool(true), ptrBool(false)},
		"all rejected": {0, 3, ptrBool(false), ptrBool(false)},
		"mixed":        {1, 3, ptrBool(true), ptrBool(true)},
		"no answers":   {0, 0, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := decideCatchAll(tc.accepted, tc.answered)
			if !sameBool(got.catchAll, tc.catchAll) || !sameBool(got.randomiser, tc.randomiser) {
				t.Errorf("decideCatchAll(%d,%d) = %v,%v", tc.accepted, tc.answered, got.catchAll, got.randomiser)
			}
		})
	}
}

func sameBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// A 250 on a catch-all domain proves nothing; the caller must be told so.
func TestCatchAllDetection(t *testing.T) {
	t.Run("catch-all accepts the bogus probe", func(t *testing.T) {
		d := scriptedMX("220 mx.test ESMTP", happyPath())
		resp := probeWith(t, d, Request{
			MXHost: "mx.test", Domain: "example.test",
			Emails: []string{"a@example.test"}, NeedCatchAll: true,
		})
		r := resp.Results["a@example.test"]
		if r.CatchAll == nil || !*r.CatchAll {
			t.Errorf("CatchAll = %v, want true", r.CatchAll)
		}
	})

	t.Run("a normal domain rejects it", func(t *testing.T) {
		// Everything not explicitly known bounces — including the random probe.
		d := scriptedMX("220 mx.test ESMTP", func(cmd string) string {
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "MAIL FROM"):
				return "250 ok"
			case strings.HasPrefix(cmd, "RCPT TO"):
				if strings.Contains(cmd, "real@") {
					return "250 2.1.5 OK"
				}
				return "550 5.1.1 No such user"
			}
			return "250 ok"
		})
		resp := probeWith(t, d, Request{
			MXHost: "mx.test", Domain: "example.test",
			Emails: []string{"real@example.test"}, NeedCatchAll: true,
		})
		r := resp.Results["real@example.test"]
		if r.CatchAll == nil || *r.CatchAll {
			t.Errorf("CatchAll = %v, want false", r.CatchAll)
		}
		if r.Accepted == nil || !*r.Accepted {
			t.Errorf("Accepted = %v, want true", r.Accepted)
		}
	})

	t.Run("not requested means not asserted", func(t *testing.T) {
		d := scriptedMX("220 mx.test ESMTP", happyPath())
		resp := probeWith(t, d, Request{
			MXHost: "mx.test", Domain: "example.test",
			Emails: []string{"a@example.test"}, NeedCatchAll: false,
		})
		if ca := resp.Results["a@example.test"].CatchAll; ca != nil {
			t.Errorf("CatchAll = %v, want nil when the caller did not ask", *ca)
		}
	})
}

func TestBatchIsSplitAcrossSessions(t *testing.T) {
	var sessions int
	d := dialFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
		sessions++
		return scriptedMX("220 mx.test ESMTP", happyPath()).DialContext(ctx, n, a)
	})
	emails := make([]string, 7)
	for i := range emails {
		emails[i] = fmt.Sprintf("u%d@example.test", i)
	}
	p := New(Options{Dialer: d, Resolver: stubResolver{}, MaxRCPTPerSession: 3, Timeout: 5 * time.Second})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: emails})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(resp.Results) != 7 {
		t.Errorf("got %d results, want 7", len(resp.Results))
	}
	if sessions != 3 { // 3 + 3 + 1
		t.Errorf("opened %d sessions, want 3", sessions)
	}
}

func TestProbeRejectsEmptyRequest(t *testing.T) {
	p := New(Options{})
	if _, err := p.Probe(t.Context(), Request{Domain: "x.test", Emails: []string{"a@x.test"}}); err == nil {
		t.Error("missing mx_host accepted")
	}
	if _, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "x.test"}); err == nil {
		t.Error("empty email list accepted")
	}
}

// Invariant 3: a bare "tcp" would leave from an IPv6 address with no FCrDNS.
func TestDialsIPv4Only(t *testing.T) {
	got := make(chan string, 1)
	d := dialFunc(func(_ context.Context, network, _ string) (net.Conn, error) {
		got <- network
		return nil, errors.New("stop here")
	})
	p := New(Options{Dialer: d, Resolver: stubResolver{}})
	if _, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "x.test", Emails: []string{"a@x.test"}}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if network := <-got; network != "tcp4" {
		t.Errorf("dialed %q, want tcp4", network)
	}
}

// Invariant 2, end to end: with no Resolver supplied the prober must still
// refuse, because the guard is the default and not something a caller opts
// into. A dialer that records any attempt proves no socket was opened.
func TestGuardRefusesInternalTargetsByDefault(t *testing.T) {
	for name, host := range map[string]string{
		"loopback":       "127.0.0.1",
		"private":        "10.0.0.5",
		"cloud metadata": "169.254.169.254",
		"unspecified":    "0.0.0.0",
		"v4-mapped v6":   "::ffff:127.0.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			dialed := false
			p := New(Options{Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("must not be reached")
			})})

			resp, err := p.Probe(t.Context(), Request{
				MXHost: host, Domain: "example.test",
				Emails: []string{"a@example.test", "b@example.test"},
			})
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if dialed {
				t.Fatal("a socket was opened to an internal address")
			}
			for addr, r := range resp.Results {
				if r.Class != ClassGuarded {
					t.Errorf("%s: Class = %s, want guarded", addr, r.Class)
				}
				// Our refusal is never a statement about the mailbox.
				if r.Accepted != nil {
					t.Errorf("%s: Accepted = %v, want nil", addr, *r.Accepted)
				}
				if r.Connected == nil || *r.Connected {
					t.Errorf("%s: Connected = %v, want false", addr, r.Connected)
				}
				if r.Err == "" {
					t.Errorf("%s: refusal carries no reason", addr)
				}
			}
		})
	}
}

// A guarded refusal is neither a throttle nor a deferral: it must not move the
// pacer, and retrying changes nothing.
func TestGuardedIsNeitherThrottleNorTemp(t *testing.T) {
	if ClassGuarded.IsThrottle() {
		t.Error("guarded moves the pacer; slowing down does not change a DNS record")
	}
	if ClassGuarded.IsTemp() {
		t.Error("guarded is retryable; the MX will still point inward tomorrow")
	}
}

// recordingPacer captures exactly what the prober is allowed to tell the pacer.
type recordingPacer struct {
	signals    []bool
	acquireErr error
	acquires   int
}

func (r *recordingPacer) Acquire(context.Context, string, string) error {
	r.acquires++
	return r.acquireErr
}

func (r *recordingPacer) Observe(_ context.Context, _ string, throttled bool) {
	r.signals = append(r.signals, throttled)
}

func (r *recordingPacer) throttles() int {
	n := 0
	for _, s := range r.signals {
		if s {
			n++
		}
	}
	return n
}

// Invariant 6, the fix this repo carries from ../ds-smtp-retry: only a genuine
// rate signal moves the pacer. Greylisting is per-recipient and
// rate-independent; a 5.7.x policy block is about our IP, and slowing down does
// not grow a PTR record. If either counted, three full mailboxes or one blocked
// IP would drag a whole provider to a crawl.
func TestOnlyRealThrottlingMovesThePacer(t *testing.T) {
	for name, tc := range map[string]struct {
		banner, rcpt  string
		wantThrottles int
	}{
		"accepted":            {"220 mx.test ESMTP", "250 2.1.5 OK", 0},
		"no such user":        {"220 mx.test ESMTP", "550 5.1.1 No such user", 0},
		"greylisted":          {"220 mx.test ESMTP", "450 4.2.0 Greylisted, try later", 0},
		"over quota 4.2.2":    {"220 mx.test ESMTP", "452 4.2.2 The email account is over quota", 0},
		"policy block on IP":  {"220 mx.test ESMTP", "554 5.7.1 Client host blocked using Spamhaus", 0},
		"reverse dns failure": {"220 mx.test ESMTP", "550 5.7.25 Forward-confirmed reverse DNS failed", 0},
		"421 at RCPT":         {"220 mx.test ESMTP", "421 4.7.0 unusual rate of traffic", 1},
		"421 in the banner":   {"421 4.7.0 unusual rate of traffic", "250 2.1.5 OK", 1},
	} {
		t.Run(name, func(t *testing.T) {
			pc := &recordingPacer{}
			d := scriptedMX(tc.banner, func(cmd string) string {
				switch {
				case strings.HasPrefix(cmd, "RCPT TO"):
					return tc.rcpt
				case strings.HasPrefix(cmd, "RSET"):
					return ""
				}
				return "250 ok"
			})
			p := New(Options{Dialer: d, Resolver: stubResolver{}, Pacer: pc, Timeout: 5 * time.Second})
			if _, err := p.Probe(t.Context(), Request{
				MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"},
			}); err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if got := pc.throttles(); got != tc.wantThrottles {
				t.Errorf("pacer saw %d throttle signals, want %d (signals: %v)", got, tc.wantThrottles, pc.signals)
			}
		})
	}
}

// Invariant 5: with no budget the probe is not sent. Not sending is a
// recoverable non-answer; sending unpaced is a blocklist entry.
func TestFailsClosedWithoutBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantClass Class
	}{
		"bucket unreachable": {errors.New("connection refused"), ClassNoBudget},
		"mx paused":          {fmt.Errorf("%w for mx.test", pacer.ErrPaused), ClassPaused},
	} {
		t.Run(name, func(t *testing.T) {
			dialed := false
			pc := &recordingPacer{acquireErr: tc.err}
			p := New(Options{
				Pacer:    pc,
				Resolver: stubResolver{},
				Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
					dialed = true
					return nil, errors.New("must not be reached")
				}),
			})

			resp, err := p.Probe(t.Context(), Request{
				MXHost: "mx.test", Domain: "example.test",
				Emails: []string{"a@example.test", "b@example.test"},
			})
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if dialed {
				t.Fatal("a probe was sent without budget")
			}
			for addr, r := range resp.Results {
				if r.Class != tc.wantClass {
					t.Errorf("%s: Class = %s, want %s", addr, r.Class, tc.wantClass)
				}
				if r.Accepted != nil {
					t.Errorf("%s: Accepted = %v, want nil", addr, *r.Accepted)
				}
			}
		})
	}
}

// The band is a rate of questions asked: batching many RCPTs down one
// connection must not spend less budget than asking them one at a time.
func TestEveryRecipientCostsAToken(t *testing.T) {
	pc := &recordingPacer{}
	d := scriptedMX("220 mx.test ESMTP", happyPath())
	p := New(Options{Dialer: d, Resolver: stubResolver{}, Pacer: pc, Timeout: 5 * time.Second})
	emails := []string{"a@example.test", "b@example.test", "c@example.test", "d@example.test"}
	if _, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: emails}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if pc.acquires != len(emails) {
		t.Errorf("took %d tokens for %d recipients, want one each", pc.acquires, len(emails))
	}
}

// policyMX answers every RCPT with a reply about the *client*, which is what a
// server that has decided against our IP does for the rest of the session.
func policyMX(exempt ...string) Dialer {
	return scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO"):
			for _, e := range exempt {
				if strings.Contains(cmd, e) {
					return "250 2.1.5 OK"
				}
			}
			return "550 5.7.25 Forward-confirmed reverse DNS failed"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	})
}

func addressList(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("u%02d@example.test", i)
	}
	return out
}

// Continuing past an unambiguous refusal spends a token per recipient on a
// question whose answer is known, and keeps hammering a server that said no.
func TestPolicyStopEndsTheSession(t *testing.T) {
	pc := &recordingPacer{}
	p := New(Options{
		Dialer: policyMX(), Resolver: stubResolver{}, Pacer: pc,
		PolicyStop: 5, CatchAllProbes: 3, Timeout: 5 * time.Second,
	})
	emails := addressList(20)

	resp, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test", Emails: emails, NeedCatchAll: true,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(resp.Results) != 20 {
		t.Fatalf("got %d results, want 20 — every address must be accounted for", len(resp.Results))
	}

	var attempted, notAttempted int
	for _, r := range resp.Results {
		if r.Class != ClassPolicy {
			t.Errorf("Class = %s, want policy throughout", r.Class)
		}
		if r.Accepted != nil {
			t.Errorf("a policy reply produced a verdict: %v", *r.Accepted)
		}
		if strings.Contains(r.Err, "not attempted") {
			notAttempted++
		} else {
			attempted++
		}
	}
	if attempted != 5 {
		t.Errorf("sent %d probes before stopping, want 5", attempted)
	}
	if notAttempted != 15 {
		t.Errorf("%d addresses marked unattempted, want 15", notAttempted)
	}
	// Budget is spent per question asked, so stopping early must save it.
	if pc.acquires != 5 {
		t.Errorf("spent %d tokens, want 5 — stopping early must stop spending", pc.acquires)
	}
	// Invariant 6: none of this is a rate signal.
	if pc.throttles() != 0 {
		t.Error("a policy block moved the pacer; slowing down does not grow a PTR record")
	}
}

// One 5.7.x among ordinary answers is a per-recipient policy — a distribution
// list rejecting external senders, say — not the server refusing the client.
func TestPolicyStopCountsConsecutiveOnly(t *testing.T) {
	// Every third address answers normally, so the run never reaches 5.
	d := scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO"):
			if strings.Contains(cmd, "3@") || strings.Contains(cmd, "6@") || strings.Contains(cmd, "9@") {
				return "250 2.1.5 OK"
			}
			return "550 5.7.1 Access denied"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	})
	p := New(Options{Dialer: d, Resolver: stubResolver{}, PolicyStop: 5, Timeout: 5 * time.Second})
	emails := addressList(12)

	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: emails})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for addr, r := range resp.Results {
		if strings.Contains(r.Err, "not attempted") {
			t.Errorf("%s was skipped; an isolated policy reply must not stop the batch", addr)
		}
	}
}

func TestPolicyStopDisabled(t *testing.T) {
	pc := &recordingPacer{}
	p := New(Options{
		Dialer: policyMX(), Resolver: stubResolver{}, Pacer: pc,
		PolicyStop: 0, Timeout: 5 * time.Second,
	})
	emails := addressList(8)
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: emails})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for addr, r := range resp.Results {
		if strings.Contains(r.Err, "not attempted") {
			t.Errorf("%s skipped with policy_stop disabled", addr)
		}
	}
	if pc.acquires != len(emails) {
		t.Errorf("spent %d tokens, want %d", pc.acquires, len(emails))
	}
}

// A server refusing us cannot tell us which local parts exist.
func TestPolicyStopSkipsCatchAllProbing(t *testing.T) {
	var bogus int
	var mu sync.Mutex
	d := scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO"):
			if !strings.Contains(cmd, "@example.test") || !strings.Contains(cmd, "u") {
				mu.Lock()
				bogus++
				mu.Unlock()
			}
			return "550 5.7.25 Forward-confirmed reverse DNS failed"
		case strings.HasPrefix(cmd, "RSET"):
			return ""
		}
		return "250 ok"
	})
	p := New(Options{
		Dialer: d, Resolver: stubResolver{}, PolicyStop: 3, CatchAllProbes: 3,
		Timeout: 5 * time.Second,
	})
	if _, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test", Emails: addressList(10), NeedCatchAll: true,
	}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bogus != 0 {
		t.Errorf("sent %d catch-all probes after the server refused us", bogus)
	}
}

// A refusal we make ourselves must be free and must touch no shared state.
// Taking a token first would spend a recipient MX's budget on a server we will
// never contact — and because mx_host is attacker-influenced, it would create a
// bucket key named after whatever the caller sent.
func TestGuardedTargetSpendsNoBudget(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.5", "localhost"} {
		t.Run(host, func(t *testing.T) {
			pc := &recordingPacer{}
			p := New(Options{
				Pacer: pc,
				Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("must not be reached")
				}),
			})
			resp, err := p.Probe(t.Context(), Request{
				MXHost: host, Domain: "evil.test",
				Emails: []string{"a@evil.test", "b@evil.test"},
			})
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			for _, r := range resp.Results {
				if r.Class != ClassGuarded {
					t.Errorf("Class = %s, want guarded", r.Class)
				}
			}
			if pc.acquires != 0 {
				t.Errorf("took %d tokens for a target we refuse to contact, want 0", pc.acquires)
			}
		})
	}
}

// The guard fires even when the budget could not have been established, so a
// Redis outage does not hide an SSRF refusal behind a fail-closed one.
func TestGuardFiresBeforeTheBudgetCheck(t *testing.T) {
	pc := &recordingPacer{acquireErr: errors.New("connection refused")}
	p := New(Options{Pacer: pc})
	resp, err := p.Probe(t.Context(), Request{
		MXHost: "127.0.0.1", Domain: "evil.test", Emails: []string{"a@evil.test"},
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := resp.Results["a@evil.test"].Class; got != ClassGuarded {
		t.Errorf("Class = %s, want guarded — the guard must not be masked by a dead bucket", got)
	}
}
