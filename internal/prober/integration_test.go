package prober_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/smtp"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// startMX runs one mxsim profile on an ephemeral port and returns host:port.
// This is the integration seam the plans name: a fake MX that answers like the
// real providers do, without touching anyone else's server.
func startMX(t *testing.T, profileName string, tweak ...func(*policy.Profile)) (host, port string, clk *clock.Offsetting) {
	t.Helper()
	p, err := policy.LoadProfile(filepath.Join("..", "..", "config", "mxsim", profileName+".yaml"))
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
	clk = clock.New()
	srv := smtp.New(policy.NewEngine(p, clk), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return host, port, clk
}

// loopbackResolver returns 127.0.0.1 without vetting it. Production must never
// do this — it is precisely the shape invariant 2 forbids — but the fake MX
// runs on loopback, so these tests bypass the guard deliberately and visibly
// rather than weakening it.
type loopbackResolver struct{}

func (loopbackResolver) Resolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func probeAgainst(t *testing.T, port string, req prober.Request) prober.Response {
	t.Helper()
	p := prober.New(prober.Options{
		Resolver:    loopbackResolver{},
		Helo:        "mail.datascoutmail.com",
		MailFrom:    "verify@probe.datascoutmail.com",
		Port:        port,
		DialNetwork: "tcp4",
		Timeout:     10 * time.Second,
	})
	resp, err := p.Probe(t.Context(), req)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return resp
}

// The plan-001 gate: one session, correct per-address answers.
func TestAgainstMxsimGmailProfile(t *testing.T) {
	host, port, _ := startMX(t, "gmail")
	resp := probeAgainst(t, port, prober.Request{
		MXHost:       host,
		Domain:       "gmail-sim.test",
		Emails:       []string{"valid@gmail-sim.test", "nope@gmail-sim.test"},
		NeedCatchAll: true,
	})

	good := resp.Results["valid@gmail-sim.test"]
	if good.Accepted == nil || !*good.Accepted {
		t.Errorf("valid@: Accepted = %v, want true (reply %q)", good.Accepted, good.Reply)
	}
	bad := resp.Results["nope@gmail-sim.test"]
	if bad.Accepted == nil || *bad.Accepted {
		t.Errorf("nope@: Accepted = %v, want false (reply %q)", bad.Accepted, bad.Reply)
	}
	if bad.EnhancedCode != "5.1.1" {
		t.Errorf("nope@: EnhancedCode = %q, want 5.1.1", bad.EnhancedCode)
	}
	// A provider that rejects unknown recipients is not a catch-all.
	if good.CatchAll == nil || *good.CatchAll {
		t.Errorf("CatchAll = %v, want false for a normal provider", good.CatchAll)
	}
}

// A 250 here proves nothing, and the caller must be told so (invariant 7).
func TestAgainstMxsimCatchAllProfile(t *testing.T) {
	host, port, _ := startMX(t, "catchall")
	resp := probeAgainst(t, port, prober.Request{
		MXHost:       host,
		Domain:       "catchall-sim.test",
		Emails:       []string{"whoever@catchall-sim.test"},
		NeedCatchAll: true,
	})

	r := resp.Results["whoever@catchall-sim.test"]
	if r.Accepted == nil || !*r.Accepted {
		t.Errorf("Accepted = %v, want true", r.Accepted)
	}
	if r.CatchAll == nil || !*r.CatchAll {
		t.Fatalf("CatchAll = %v, want true — a 250 from this host means nothing", r.CatchAll)
	}
}

// Throttling arrives in the banner, before MAIL FROM. It can never be a
// statement about a mailbox (invariant 1) but it is the one signal that moves
// the pacer (invariant 6).
func TestAgainstMxsimThrottleIsNeverInvalid(t *testing.T) {
	host, port, _ := startMX(t, "gmail") // conn_rate 10 per 60s

	var throttled bool
	for i := range 14 {
		resp := probeAgainst(t, port, prober.Request{
			MXHost: host,
			Domain: "gmail-sim.test",
			Emails: []string{"valid@gmail-sim.test"},
		})
		r := resp.Results["valid@gmail-sim.test"]
		if r.Class != prober.ClassThrottled {
			continue
		}
		throttled = true
		if r.Accepted != nil {
			t.Errorf("attempt %d: throttled but Accepted = %v, want nil", i, *r.Accepted)
		}
		if r.Connected == nil || *r.Connected {
			t.Errorf("attempt %d: throttled but Connected = %v, want false", i, r.Connected)
		}
		if !r.Class.IsThrottle() {
			t.Error("a 421 must move the pacer")
		}
		break
	}
	if !throttled {
		t.Fatal("the profile never throttled; the rate ceiling was not reached")
	}
}

// Greylisting is a 4xx on first sight of a (sender, recipient, IP) tuple that
// clears when the same tuple comes back. The retry queue is the caller's
// (plan 006); what this service owes it is a deferral it can schedule against.
func TestAgainstMxsimGreylisting(t *testing.T) {
	host, port, clk := startMX(t, "yahoo", func(p *policy.Profile) {
		// The profile tarpits the banner for three seconds on purpose; that is
		// a different behaviour's test, and paying it twice here buys nothing.
		p.Behaviour.TarpitBanner = 0
		p.Behaviour.TarpitRcpt = 0
	})

	req := prober.Request{
		MXHost: host, Domain: "yahoo-sim.test",
		Emails: []string{"valid@yahoo-sim.test"},
	}

	first := probeAgainst(t, port, req).Results["valid@yahoo-sim.test"]
	if first.Class != prober.ClassDeferred {
		t.Fatalf("first sighting: class = %s, want deferred (reply %q)", first.Class, first.Reply)
	}
	if first.Accepted != nil {
		t.Errorf("a greylisted address must not carry a verdict: Accepted = %v", *first.Accepted)
	}
	if first.RetryAfterSeconds <= 0 {
		t.Error("no retry hint; the caller would have to guess when the window opens")
	}

	// The caller comes back after the window — same sender, same recipient,
	// same IP, which is what makes the tuple match.
	clk.Advance(6 * time.Minute)

	second := probeAgainst(t, port, req).Results["valid@yahoo-sim.test"]
	if second.Accepted == nil || !*second.Accepted {
		t.Fatalf("after the window: Accepted = %v, class %s, reply %q",
			second.Accepted, second.Class, second.Reply)
	}
	if second.RetryAfterSeconds != 0 {
		t.Errorf("an answered address carries a retry hint: %d", second.RetryAfterSeconds)
	}
}
