package relay

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// transcript records the session. Mutex-guarded because the fake server writes
// from its own goroutine while the test reads — `strings.Builder` is not safe
// for that, and the race detector is right to say so.
type transcript struct {
	mu sync.Mutex
	b  strings.Builder
}

func (t *transcript) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b.WriteString(line + "\n")
}

func (t *transcript) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

// fakeMX plays one SMTP session from a script and records what it was told.
func fakeMX(t *testing.T, reply func(cmd string) string) (net.Conn, *transcript) {
	t.Helper()
	client, server := net.Pipe()
	got := &transcript{}
	go func() {
		defer func() { _ = server.Close() }()
		r := bufio.NewReader(server)
		_, _ = server.Write([]byte("220 mx.test ESMTP\r\n"))
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				got.add(line)
				if line == "." {
					inData = false
					_, _ = server.Write([]byte("250 2.0.0 Ok: queued\r\n"))
				}
				continue
			}
			got.add(line)
			if strings.HasPrefix(line, "DATA") {
				inData = true
				_, _ = server.Write([]byte("354 End data with <CR><LF>.<CR><LF>\r\n"))
				continue
			}
			out := reply(line)
			if out == "" {
				return
			}
			_, _ = server.Write([]byte(out + "\r\n"))
		}
	}()
	return client, got
}

func plainMX(cmd string) string {
	switch {
	case strings.HasPrefix(cmd, "EHLO"):
		return "250-mx.test\r\n250 SIZE 10240000"
	case strings.HasPrefix(cmd, "QUIT"):
		return "221 2.0.0 Bye"
	default:
		return "250 2.1.0 Ok"
	}
}

func TestSendDeliversAndReportsTheReply(t *testing.T) {
	conn, transcript := fakeMX(t, plainMX)
	out := Send(t.Context(), conn, SendOptions{
		MXHost: "mx.test", Helo: "mail.test",
		MailFrom: "bounces+abc@datascoutmail.com", RcptTo: "someone@example.com",
		Data: []byte("Subject: hi\r\n\r\nbody\r\n"), Timeout: 5 * time.Second,
	})
	if !out.Delivered || out.Code != 250 {
		t.Fatalf("Outcome = %+v, want delivered", out)
	}
	if !strings.Contains(transcript.String(), "MAIL FROM:<bounces+abc@datascoutmail.com>") {
		t.Errorf("the VERP return path never reached the wire:\n%s", transcript.String())
	}
}

// A body line beginning with a dot ends the message early unless it is doubled,
// and everything after it is read as SMTP commands. That is how a message body
// becomes command injection.
func TestALeadingDotInTheBodyIsStuffed(t *testing.T) {
	conn, transcript := fakeMX(t, plainMX)
	out := Send(t.Context(), conn, SendOptions{
		MXHost: "mx.test", Helo: "mail.test",
		MailFrom: "a@test", RcptTo: "b@test",
		Data:    []byte("Subject: hi\r\n\r\n.\r\n.QUIT\r\nlast line\r\n"),
		Timeout: 5 * time.Second,
	})
	if !out.Delivered {
		t.Fatalf("Outcome = %+v", out)
	}
	body := transcript.String()
	if !strings.Contains(body, "\n..\n") {
		t.Errorf("a bare dot line was not doubled:\n%s", body)
	}
	if !strings.Contains(body, "\n..QUIT\n") {
		t.Errorf("a dot-prefixed line was not stuffed — the rest would be read as commands:\n%s", body)
	}
	if !strings.Contains(body, "\nlast line\n") {
		t.Error("the message was truncated at the dot")
	}
}

func TestStartTLSIsUsedWhenOffered(t *testing.T) {
	// The capability sits on a *continuation* line, which is the whole reason
	// this package reads every line of a reply rather than only the last.
	conn, transcript := fakeMX(t, func(cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			return "250-mx.test\r\n250-STARTTLS\r\n250 SIZE 10240000"
		case strings.HasPrefix(cmd, "STARTTLS"):
			// Accept, then hang up: the handshake cannot complete against this
			// fake, and a transport failure here is the correct outcome — what
			// is being asserted is that STARTTLS was *attempted*.
			return "220 2.0.0 Ready to start TLS"
		default:
			return "250 2.1.0 Ok"
		}
	})
	out := Send(t.Context(), conn, SendOptions{
		MXHost: "mx.test", Helo: "mail.test", MailFrom: "a@test", RcptTo: "b@test",
		Data: []byte("Subject: hi\r\n\r\nbody\r\n"), Timeout: 3 * time.Second,
	})
	if !strings.Contains(transcript.String(), "STARTTLS") {
		t.Fatalf("STARTTLS was offered and never attempted:\n%s", transcript.String())
	}
	if out.Delivered {
		t.Error("a failed TLS handshake must not report delivery")
	}
}

// A 5xx is final: retrying spends the IP's standing on a question already
// answered. A 4xx is not, and the queue must be able to tell them apart.
func TestPermanentAndTransientAreDistinguished(t *testing.T) {
	for _, c := range []struct {
		name      string
		reply     string
		permanent bool
	}{
		{"hard rejection", "550 5.1.1 No such user", true},
		{"greylisted", "451 4.7.1 Try again later", false},
		{"throttled", "421 4.7.0 Too many connections", false},
	} {
		conn, _ := fakeMX(t, func(cmd string) string {
			if strings.HasPrefix(cmd, "RCPT") {
				return c.reply
			}
			return plainMX(cmd)
		})
		out := Send(t.Context(), conn, SendOptions{
			MXHost: "mx.test", Helo: "mail.test", MailFrom: "a@test", RcptTo: "b@test",
			Data: []byte("Subject: hi\r\n\r\nbody\r\n"), Timeout: 5 * time.Second,
		})
		if out.Delivered {
			t.Errorf("%s: reported delivered", c.name)
		}
		if out.Permanent != c.permanent {
			t.Errorf("%s: Permanent = %v, want %v (reply %q)", c.name, out.Permanent, c.permanent, c.reply)
		}
	}
}
