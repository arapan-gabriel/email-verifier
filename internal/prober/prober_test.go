package prober

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
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
	p := New(Options{Dialer: d, Timeout: 5 * time.Second, Helo: "mail.test", MailFrom: "verify@probe.test"})
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
	p := New(Options{Dialer: d, MaxRCPTPerSession: 3, Timeout: 5 * time.Second})
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
	p := New(Options{Dialer: d})
	if _, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "x.test", Emails: []string{"a@x.test"}}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if network := <-got; network != "tcp4" {
		t.Errorf("dialed %q, want tcp4", network)
	}
}
