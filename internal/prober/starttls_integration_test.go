package prober_test

import (
	"context"
	"crypto/x509"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/smtp"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// STARTTLS against mxsim (plan 031). Every path a real MX can take: not
// offered, offered, required before MAIL FROM, and agreed-then-broken.

func withTLS(mode string) func(*policy.Profile) {
	return func(p *policy.Profile) { p.StartTLS = mode }
}

// countingDialer counts sockets, so a test can prove the TLS layer reused the
// vetted connection instead of dialling again (invariant 2).
type countingDialer struct{ n atomic.Int64 }

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.n.Add(1)
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

// throttleWatch records every Observe so a test can assert a TLS failure never
// reached the pacer as a rate signal (invariant 6).
type throttleWatch struct {
	mu        sync.Mutex
	throttled int
}

func (w *throttleWatch) Acquire(context.Context, string, string) error { return nil }
func (w *throttleWatch) Observe(_ context.Context, _ string, throttled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if throttled {
		w.throttled++
	}
}

type tlsCounter struct {
	mu       sync.Mutex
	outcomes map[string]int
}

func (c *tlsCounter) Result(string)     {}
func (c *tlsCounter) Reply(int, string) {}
func (c *tlsCounter) Blocked(string)    {}
func (c *tlsCounter) TLSSession(o string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcomes == nil {
		c.outcomes = map[string]int{}
	}
	c.outcomes[o]++
}

type tlsRig struct {
	dialer  *countingDialer
	pacer   *throttleWatch
	metrics *tlsCounter
}

func probeTLS(t *testing.T, port string, req prober.Request, tweak func(*prober.Options)) (prober.Response, *tlsRig) {
	t.Helper()
	rig := &tlsRig{dialer: &countingDialer{}, pacer: &throttleWatch{}, metrics: &tlsCounter{}}
	opts := prober.Options{
		Resolver:    loopbackResolver{},
		Dialer:      rig.dialer,
		Pacer:       rig.pacer,
		Metrics:     rig.metrics,
		Helo:        "mail.datascoutmail.com",
		MailFrom:    "verify@probe.datascoutmail.com",
		Port:        port,
		DialNetwork: "tcp4",
		Timeout:     10 * time.Second,
	}
	if tweak != nil {
		tweak(&opts)
	}
	resp, err := prober.New(opts).Probe(t.Context(), req)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return resp, rig
}

func gmailReq(host string) prober.Request {
	return prober.Request{
		MXHost: host, Domain: "gmail-sim.test",
		Emails: []string{"valid@gmail-sim.test", "nope@gmail-sim.test"},
	}
}

// Offered → upgraded, re-EHLO'd and answered. mxsim forgets the pre-TLS EHLO,
// so a valid answer is itself the proof that the EHLO was repeated over TLS.
func TestOfferedSTARTTLSIsUsedAndTheSessionAnswers(t *testing.T) {
	host, port, _ := startMX(t, "gmail", withTLS("offered"))
	resp, rig := probeTLS(t, port, gmailReq(host), nil)

	good, bad := resp.Results["valid@gmail-sim.test"], resp.Results["nope@gmail-sim.test"]
	if good.Accepted == nil || !*good.Accepted || bad.Accepted == nil || *bad.Accepted {
		t.Fatalf("answers over TLS: valid=%+v nope=%+v", good, bad)
	}
	// A self-signed certificate that names neither the MX nor anything trusted
	// is recorded, never refused.
	for addr, r := range resp.Results {
		if r.TLS != prober.TLSUnverified {
			t.Errorf("%s: TLS = %q, want %q", addr, r.TLS, prober.TLSUnverified)
		}
	}
	if n := rig.dialer.n.Load(); n != 1 {
		t.Errorf("dialled %d times, want 1: the upgrade must reuse the vetted connection", n)
	}
	if rig.metrics.outcomes[prober.TLSUnverified] != 1 {
		t.Errorf("tls outcomes = %v, want one unverified session", rig.metrics.outcomes)
	}
}

// Not offered → plaintext, exactly as before plan 031.
func TestNoSTARTTLSStaysPlaintext(t *testing.T) {
	host, port, _ := startMX(t, "gmail")
	resp, rig := probeTLS(t, port, gmailReq(host), nil)
	if r := resp.Results["valid@gmail-sim.test"]; r.Accepted == nil || !*r.Accepted || r.TLS != prober.TLSNone {
		t.Fatalf("plaintext session: %+v, want accepted with TLS none", r)
	}
	if rig.metrics.outcomes[prober.TLSNone] != 1 {
		t.Errorf("tls outcomes = %v, want one none", rig.metrics.outcomes)
	}
}

// Required → a host that refuses MAIL FROM in plaintext answers once upgraded.
// This is the ladder's 530 "Must issue a STARTTLS command first" turning into
// an answer.
func TestARequiredUpgradeTurnsARefusalIntoAnAnswer(t *testing.T) {
	host, port, _ := startMX(t, "gmail", withTLS("required"))

	resp, _ := probeTLS(t, port, gmailReq(host), nil)
	if r := resp.Results["valid@gmail-sim.test"]; r.Accepted == nil || !*r.Accepted {
		t.Fatalf("required host with STARTTLS on: %+v, want accepted", r)
	}

	off, rig := probeTLS(t, port, gmailReq(host), func(o *prober.Options) { o.StartTLS = "off" })
	r := off.Results["valid@gmail-sim.test"]
	if r.Accepted != nil || r.Class != prober.ClassPolicy || r.TLS != prober.TLSNone {
		t.Fatalf("required host with STARTTLS off: %+v, want the 530 classed policy, no verdict", r)
	}
	if rig.pacer.throttled != 0 {
		t.Errorf("a TLS demand reached the pacer as %d throttle signals", rig.pacer.throttled)
	}
}

// Agreed, then broken → tls_failed: no verdict, a retry hint, no throttle, and
// no second plaintext session.
func TestABrokenHandshakeIsTLSFailedAndNeverAVerdict(t *testing.T) {
	host, port, _ := startMX(t, "gmail", withTLS("broken"))
	resp, rig := probeTLS(t, port, gmailReq(host), nil)
	for addr, r := range resp.Results {
		if r.Class != prober.ClassTLSFailed || r.Accepted != nil || r.TLS != prober.TLSFailed {
			t.Errorf("%s: %+v, want tls_failed with no verdict", addr, r)
		}
		if r.RetryAfterSeconds <= 0 {
			t.Errorf("%s: no retry hint on tls_failed", addr)
		}
	}
	if rig.pacer.throttled != 0 {
		t.Errorf("tls_failed reached the pacer as %d throttle signals", rig.pacer.throttled)
	}
	if n := rig.dialer.n.Load(); n != 1 {
		t.Errorf("dialled %d times: a failed upgrade must not retry in plaintext", n)
	}
	if prober.ClassTLSFailed.IsThrottle() || !prober.ClassTLSFailed.IsTemp() {
		t.Error("tls_failed must be temporary and never a throttle")
	}
}

// A certificate that chains to a trusted root and names the MX is "verified".
func TestATrustedCertificateIsVerified(t *testing.T) {
	_, port, _ := startMX(t, "gmail", withTLS("offered"))
	cert, err := smtp.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	resp, _ := probeTLS(t, port, gmailReq("mxsim.invalid"), func(o *prober.Options) { o.TLSRootCAs = roots })
	if r := resp.Results["valid@gmail-sim.test"]; r.TLS != prober.TLSVerified {
		t.Fatalf("trusted, matching certificate: TLS = %q, want verified", r.TLS)
	}
}
