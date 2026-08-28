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
func startMX(t *testing.T, profileName string) (host, port string) {
	t.Helper()
	p, err := policy.LoadProfile(filepath.Join("..", "..", "config", "mxsim", profileName+".yaml"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.New(policy.NewEngine(p, clock.New()), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	return host, port
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
	host, port := startMX(t, "gmail")
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
	host, port := startMX(t, "catchall")
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
	host, port := startMX(t, "gmail") // conn_rate 10 per 60s

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
