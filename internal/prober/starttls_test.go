package prober

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errNoBudgetForTest = errors.New("redis: connection refused")

func TestAdvertisesSTARTTLS(t *testing.T) {
	for _, tc := range []struct {
		ehlo string
		want bool
	}{
		{"250-mx.example\n250-PIPELINING\n250-STARTTLS\n250 8BITMIME", true},
		{"250-mx.example\n250 starttls", true},
		{"250-mx.example\n250-PIPELINING\n250 8BITMIME", false},
		// The greeting line is the server's name, never a capability.
		{"250 STARTTLS.example.com", false},
		// A keyword that merely contains the word is not the extension.
		{"250-mx.example\n250 X-STARTTLS-LATER", false},
		{"", false},
	} {
		if got := advertisesSTARTTLS(tc.ehlo); got != tc.want {
			t.Errorf("advertisesSTARTTLS(%q) = %v, want %v", tc.ehlo, got, tc.want)
		}
	}
}

// ehloWithTLS answers like a server that offers STARTTLS, and records whether
// the prober ever asked for it.
func ehloWithTLS(asked *atomic.Bool, startTLSReply string) Dialer {
	return scriptedMX("220 mx.test ESMTP", func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			return "250-mx.test\r\n250-PIPELINING\r\n250 STARTTLS"
		case cmd == "STARTTLS":
			asked.Store(true)
			return startTLSReply
		case strings.HasPrefix(cmd, "MAIL"):
			return "250 2.1.0 Ok"
		case strings.HasPrefix(cmd, "RCPT"):
			return "250 2.1.5 Ok"
		}
		return ""
	})
}

// A server that advertises STARTTLS and then declines it is answered in
// plaintext on the same connection (RFC 3207 §4: the client may continue
// unencrypted), like Postfix at `may`. Nothing is remembered: a 454 is a
// "not now", and the next session asks again.
func TestADeclinedSTARTTLSFallsBackOnTheSameSession(t *testing.T) {
	var asked atomic.Bool
	pc := &recordingPacer{}
	prof := newTLSProfiles()
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "454 4.7.0 TLS not available due to local problem"),
		Resolver: stubResolver{}, Pacer: pc, Profiles: prof, Timeout: 5 * time.Second,
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Results["a@example.test"]
	if r.Class != ClassValid || r.Accepted == nil || !*r.Accepted || r.TLS != TLSFallback {
		t.Fatalf("declined STARTTLS: %+v, want a plaintext verdict with tls=fallback", r)
	}
	if !asked.Load() {
		t.Error("STARTTLS was never asked")
	}
	if pc.throttles() != 0 {
		t.Errorf("a declined STARTTLS was observed as %d throttles", pc.throttles())
	}
	if pc.acquires != 1 {
		t.Errorf("same-session fallback took %d tokens, want 1 (no redial)", pc.acquires)
	}
	if prof.isBroken("mx.test") {
		t.Error("a 454 marked the host TLS-broken; only a failed handshake may")
	}
}

// The handshake is bounded on its own: a server that says 220 and then never
// speaks TLS costs the handshake timeout, not the whole session — and the
// plaintext rerun still produces the verdict.
func TestTheHandshakeRespectsItsTimeout(t *testing.T) {
	var asked atomic.Bool
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "220 2.0.0 Ready to start TLS"),
		Resolver: stubResolver{}, Timeout: 5 * time.Second,
		TLSHandshakeTimeout: 200 * time.Millisecond,
	})
	start := time.Now()
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("handshake took %s, want it bounded by the 200ms handshake timeout", elapsed)
	}
	if r := resp.Results["a@example.test"]; r.Class != ClassValid || r.TLS != TLSFallback {
		t.Fatalf("silent handshake: %+v, want the plaintext rerun's verdict with tls=fallback", r)
	}
}

// tlsProfiles is fakeProfiles plus the TLS-broken memory (TLSProfiles).
type tlsProfiles struct {
	*fakeProfiles
	mu     sync.Mutex
	broken map[string]bool
	down   bool // the store is unreachable: reads say "not broken", writes vanish
	marks  int
}

func newTLSProfiles(broken ...string) *tlsProfiles {
	t := &tlsProfiles{fakeProfiles: newProfiles(), broken: map[string]bool{}}
	for _, h := range broken {
		t.broken[h] = true
	}
	return t
}

func (t *tlsProfiles) TLSBroken(_ context.Context, mxHost string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.down && t.broken[mxHost]
}

func (t *tlsProfiles) MarkTLSBroken(_ context.Context, mxHost string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.marks++
	if !t.down {
		t.broken[mxHost] = true
	}
}

func (t *tlsProfiles) isBroken(mxHost string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.broken[mxHost]
}

// tlsOutcomes records the TLSRecorder side of Metrics.
type tlsOutcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *tlsOutcomes) Result(string)     {}
func (o *tlsOutcomes) Reply(int, string) {}
func (o *tlsOutcomes) Blocked(string)    {}
func (o *tlsOutcomes) TLSSession(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n == nil {
		o.n = map[string]int{}
	}
	o.n[outcome]++
}

// countingDialer counts the connections a Dialer opens.
func countingDialer(d Dialer, n *atomic.Int32) Dialer {
	return dialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		n.Add(1)
		return d.DialContext(ctx, network, addr)
	})
}

// A host remembered as TLS-broken is answered in plaintext without asking:
// one connection, no STARTTLS on the wire, tls=skipped.
func TestARememberedHostSkipsTLS(t *testing.T) {
	var asked atomic.Bool
	var dials atomic.Int32
	pc := &recordingPacer{}
	p := New(Options{
		Dialer:   countingDialer(ehloWithTLS(&asked, "220 2.0.0 Ready"), &dials),
		Resolver: stubResolver{}, Pacer: pc, Profiles: newTLSProfiles("mx.test"), Timeout: 5 * time.Second,
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test", "b@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Load() {
		t.Fatal("STARTTLS was sent to a host remembered as TLS-broken")
	}
	for _, a := range []string{"a@example.test", "b@example.test"} {
		if r := resp.Results[a]; r.Class != ClassValid || r.TLS != TLSSkipped {
			t.Fatalf("%s: %+v, want a plaintext verdict with tls=skipped", a, r)
		}
	}
	if dials.Load() != 1 {
		t.Errorf("dialled %d times, want 1", dials.Load())
	}
}

// When the store holding the memory is down, the session behaves as before
// the memory existed: it tries TLS, and the failed write fails nothing.
func TestTLSMemoryDownStillTriesTLS(t *testing.T) {
	var asked atomic.Bool
	prof := newTLSProfiles("mx.test")
	prof.down = true
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "454 4.7.0 not now"),
		Resolver: stubResolver{}, Profiles: prof, Timeout: 5 * time.Second,
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if !asked.Load() {
		t.Fatal("with the memory unreachable, STARTTLS must still be tried")
	}
	if r := resp.Results["a@example.test"]; r.Class != ClassValid {
		t.Fatalf("memory down: %+v, want the probe answered", r)
	}
}

// A handshake that fails and a plaintext rerun that cannot connect either is
// tls_failed: no verdict, the handshake error kept, a retry hint set.
func TestAFailedHandshakeAndAFailedRerunIsTLSFailed(t *testing.T) {
	var dials atomic.Int32
	var asked atomic.Bool
	inner := ehloWithTLS(&asked, "220 2.0.0 Ready to start TLS")
	pc := &recordingPacer{}
	prof := newTLSProfiles()
	m := &tlsOutcomes{}
	p := New(Options{
		Metrics: m,
		Dialer: dialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, errors.New("connection refused")
			}
			return inner.DialContext(ctx, network, addr)
		}),
		Resolver: stubResolver{}, Pacer: pc, Profiles: prof, Timeout: 5 * time.Second,
		TLSHandshakeTimeout: 200 * time.Millisecond,
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Results["a@example.test"]
	if r.Class != ClassTLSFailed || r.Accepted != nil || r.TLS != TLSFailed || r.RetryAfterSeconds == 0 {
		t.Fatalf("handshake and rerun both failed: %+v, want tls_failed with a retry hint and no verdict", r)
	}
	if !strings.Contains(r.Err, "tls handshake") || !strings.Contains(r.Err, "plaintext retry") {
		t.Errorf("err %q should name both failures", r.Err)
	}
	if dials.Load() != 2 {
		t.Errorf("dialled %d times, want exactly one plaintext retry", dials.Load())
	}
	if !prof.isBroken("mx.test") {
		t.Error("the failed handshake was not remembered")
	}
	if pc.throttles() != 0 {
		t.Errorf("a TLS failure was observed as %d throttles", pc.throttles())
	}
	if len(m.n) != 1 || m.n[TLSFailed] != 1 {
		t.Errorf("tls outcomes = %v, want exactly one failed session", m.n)
	}
}

// fakeLeaser is a pacer with a one-slot session lease that fails a test instead of
// deadlocking when it is asked for a second slot while the first is held.
type fakeLeaser struct {
	recordingPacer
	mu       sync.Mutex
	held     bool
	acquired int
	overlap  bool
}

func (l *fakeLeaser) AcquireSession(context.Context, string, string) (func(), time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		l.overlap = true
		return nil, 0, errors.New("lease held: the rerun would wait for itself")
	}
	l.held = true
	l.acquired++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.held = false
			l.mu.Unlock()
		})
	}, 0, nil
}

// Under a one-session lease (plan 028) the rerun gives the first lease back
// before it takes its own, so a fallback never waits for itself.
func TestTheRerunReleasesTheLeaseFirst(t *testing.T) {
	var asked atomic.Bool
	l := &fakeLeaser{}
	paced := 0
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "220 2.0.0 Ready to start TLS"),
		Resolver: stubResolver{}, Pacer: l, Timeout: 5 * time.Second,
		TLSHandshakeTimeout: 200 * time.Millisecond,
		OnPaced:             func(string) { paced++ },
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test", "b@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if l.overlap {
		t.Fatal("the rerun asked for a lease while the failed session still held one")
	}
	if l.acquired != 2 || l.held {
		t.Errorf("leases acquired %d, held at end %v; want 2 and released", l.acquired, l.held)
	}
	if r := resp.Results["a@example.test"]; r.Class != ClassValid || r.TLS != TLSFallback {
		t.Fatalf("leased fallback: %+v", r)
	}
	// Every token the pacer granted — the failed session's first and every
	// RCPT of the rerun — is reported, so the warm-up counts what it spent.
	if paced != l.acquires || l.acquires < 3 {
		t.Errorf("OnPaced fired %d times for %d granted tokens (want ≥ 3: failed session + 2 RCPTs)", paced, l.acquires)
	}
}

// "off" is the escape hatch: the extension is ignored and the session runs in
// plaintext, exactly as before plan 031.
func TestStartTLSOffNeverAsks(t *testing.T) {
	var asked atomic.Bool
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "220 2.0.0 Ready"),
		Resolver: stubResolver{}, Timeout: 5 * time.Second, StartTLS: "off",
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Load() {
		t.Fatal("STARTTLS was sent with probe.starttls = off")
	}
	if r := resp.Results["a@example.test"]; r.Accepted == nil || !*r.Accepted || r.TLS != TLSNone {
		t.Fatalf("plaintext session with STARTTLS off: %+v", r)
	}
}

// A refusal we make before any socket carries no TLS field at all: there was
// no session to be encrypted or not.
func TestNoSessionMeansNoTLSField(t *testing.T) {
	p := New(Options{Dialer: ehloWithTLS(&atomic.Bool{}, ""), Resolver: stubResolver{},
		Pacer: &recordingPacer{acquireErr: errNoBudgetForTest}, Timeout: time.Second})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.Results["a@example.test"]; r.TLS != "" {
		t.Errorf("no session, but TLS = %q", r.TLS)
	}
}
