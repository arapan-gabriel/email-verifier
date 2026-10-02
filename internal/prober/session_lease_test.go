package prober

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
)

// blockedResolver refuses every MX as the SSRF guard does.
type blockedResolver struct{}

func (blockedResolver) Resolve(context.Context, string) ([]netip.Addr, error) {
	return nil, resolver.ErrNoRoutableAddress
}

// leasingPacer is a Pacer that also leases sessions, counting both sides and
// how many tokens were taken while a lease was held.
type leasingPacer struct {
	mu          sync.Mutex
	leases      int
	releases    int
	held        int
	tokensHeld  int // tokens granted while a lease was held
	tokensFree  int // tokens granted with no lease held
	leaseErr    error
	acquireErr  error
	leaseWaited time.Duration
}

func (l *leasingPacer) AcquireSession(context.Context, string, string) (func(), time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.leaseErr != nil {
		return nil, 0, l.leaseErr
	}
	l.leases++
	l.held++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releases++
		l.held--
	}, l.leaseWaited, nil
}

func (l *leasingPacer) Acquire(context.Context, string, string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.acquireErr != nil {
		return l.acquireErr
	}
	if l.held > 0 {
		l.tokensHeld++
	} else {
		l.tokensFree++
	}
	return nil
}

func (l *leasingPacer) Observe(context.Context, string, bool) {}

func (l *leasingPacer) balanced(t *testing.T, wantLeases int) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.leases != wantLeases || l.releases != l.leases || l.held != 0 {
		t.Errorf("leases %d, releases %d, held %d — want %d taken and every one given back",
			l.leases, l.releases, l.held, wantLeases)
	}
}

func probeLeased(t *testing.T, pc Pacer, d Dialer, extra func(*Options), emails ...string) Response {
	t.Helper()
	opts := Options{Dialer: d, Resolver: stubResolver{}, Pacer: pc, Timeout: 5 * time.Second,
		Rand: func() float64 { return 0.99 }}
	if extra != nil {
		extra(&opts)
	}
	resp, err := New(opts).Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test", NeedCatchAll: true, Emails: emails,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A whole session — the real RCPT and plan 029's catch-all question — runs under
// one lease, and every token is taken while it is held.
func TestOneSessionOneLeaseCoveringTheCatchAllQuestion(t *testing.T) {
	pc := &leasingPacer{}
	mx := &bogusMX{real: acceptAll, bogus: []string{accept}} // catch-all: 2 bogus RCPTs
	probeLeased(t, pc, mx.dialer(), nil, "a@example.test")
	pc.balanced(t, 1)
	if pc.tokensFree != 0 || pc.tokensHeld != 3 {
		t.Errorf("tokens held %d free %d, want 3 (1 real + 2 bogus) all under the lease", pc.tokensHeld, pc.tokensFree)
	}
}

// Every early exit gives the lease back.
func TestEveryExitGivesTheLeaseBack(t *testing.T) {
	for name, d := range map[string]Dialer{
		"dial fails": dialFunc(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		}),
		"banner refuses": scriptedMX("554 5.7.1 go away", func(string) string { return "" }),
		"MAIL FROM refused": scriptedMX("220 hi", func(cmd string) string {
			if len(cmd) >= 4 && cmd[:4] == "MAIL" {
				return "550 5.7.1 sender rejected"
			}
			return "250 ok"
		}),
		"server hangs up": dialFunc(func(context.Context, string, string) (net.Conn, error) {
			c, s := net.Pipe()
			_ = s.Close()
			return c, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			pc := &leasingPacer{}
			probeLeased(t, pc, d, nil, "a@example.test")
			pc.balanced(t, 1)
		})
	}
}

// Invariant 5: a lease set that cannot be read means no socket at all.
func TestNoLeaseNoDial(t *testing.T) {
	var dials atomic.Int64
	d := dialFunc(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, net.ErrClosed
	})
	pc := &leasingPacer{leaseErr: errors.New("redis down")}
	resp := probeLeased(t, pc, d, nil, "a@example.test", "b@example.test")
	if dials.Load() != 0 {
		t.Fatalf("dialled %d times without a lease", dials.Load())
	}
	for addr, r := range resp.Results {
		if r.Class != ClassNoBudget || r.Accepted != nil {
			t.Errorf("%s: %+v, want no_budget and no verdict", addr, r)
		}
	}
}

// A wait past the bound comes back unattempted with an exact retry hint — the
// paused path, never a verdict.
func TestALeaseTimeoutIsAPauseWithAHint(t *testing.T) {
	var dials atomic.Int64
	d := dialFunc(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, net.ErrClosed
	})
	pc := &leasingPacer{leaseErr: &pacer.PausedError{MXHost: "@microsoft-eop", Until: time.Now().Add(30 * time.Second)}}
	resp := probeLeased(t, pc, d, nil, "a@example.test")
	r := resp.Results["a@example.test"]
	if dials.Load() != 0 || r.Class != ClassPaused || r.Accepted != nil {
		t.Fatalf("dials %d, result %+v — want paused, no verdict, no socket", dials.Load(), r)
	}
	// The earliest expiry is 30 s away; hints are clamped to [60 s, 6 h] (retry.go),
	// so the caller is told to come back in a minute.
	if r.RetryAfterSeconds != int(minRetryHint.Seconds()) {
		t.Errorf("retry hint %ds, want the clamped %ds", r.RetryAfterSeconds, int(minRetryHint.Seconds()))
	}
}

// A refusal we make ourselves stays free: a guarded MX never takes a lease.
func TestAGuardedMXTakesNoLease(t *testing.T) {
	pc := &leasingPacer{}
	opts := func(o *Options) { o.Resolver = blockedResolver{} }
	probeLeased(t, pc, scriptedMX("220 hi", happyPath()), opts, "a@example.test")
	pc.balanced(t, 0)
}

func TestOnLeaseReportsTheWait(t *testing.T) {
	pc := &leasingPacer{leaseWaited: 1500 * time.Millisecond}
	var got time.Duration
	var host string
	probeLeased(t, pc, scriptedMX("220 hi", happyPath()), func(o *Options) {
		o.OnLease = func(h string, w time.Duration) { host, got = h, w }
	}, "a@example.test")
	if host != "mx.test" || got != 1500*time.Millisecond {
		t.Errorf("OnLease(%q, %s), want the real host and the wait", host, got)
	}
}
