package prober

import (
	"errors"
	"strings"
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

// A server that advertises STARTTLS and then declines it ends the session as
// tls_failed — no verdict, no throttle signal, no plaintext fallback.
func TestADeclinedSTARTTLSIsTLSFailed(t *testing.T) {
	var asked atomic.Bool
	pc := &recordingPacer{}
	p := New(Options{
		Dialer:   ehloWithTLS(&asked, "454 4.7.0 TLS not available due to local problem"),
		Resolver: stubResolver{}, Pacer: pc, Timeout: 5 * time.Second,
	})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Results["a@example.test"]
	if r.Class != ClassTLSFailed || r.Accepted != nil || r.TLS != TLSFailed || r.SMTPCode != 454 {
		t.Fatalf("declined STARTTLS: %+v, want tls_failed with the 454 kept and no verdict", r)
	}
	if pc.throttles() != 0 {
		t.Errorf("a declined STARTTLS was observed as %d throttles", pc.throttles())
	}
}

// The handshake is bounded on its own: a server that says 220 and then never
// speaks TLS costs the handshake timeout, not the whole session.
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
	if r := resp.Results["a@example.test"]; r.Class != ClassTLSFailed || r.Accepted != nil {
		t.Fatalf("silent handshake: %+v, want tls_failed", r)
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
