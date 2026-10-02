package prober

import (
	"strings"
	"testing"
	"time"
)

// Plan 032 sees every refusal of us — at RCPT and before it — and nothing else.
func TestOnRefusalSeesPolicyRepliesOnly(t *testing.T) {
	type seen struct{ host, reply string }
	run := func(banner string, rcpt string) []seen {
		var got []seen
		d := scriptedMX(banner, func(cmd string) string {
			switch {
			case strings.HasPrefix(cmd, "RCPT TO"):
				return rcpt
			case strings.HasPrefix(cmd, "RSET"):
				return ""
			}
			return "250 ok"
		})
		p := New(Options{Dialer: d, Resolver: stubResolver{}, Timeout: 5 * time.Second,
			OnRefusal: func(h, r string) { got = append(got, seen{h, r}) }})
		_, _ = p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test",
			Emails: []string{"a@example.test"}})
		return got
	}

	if got := run("220 ok", "550 5.7.1 Client host [92.222.87.97] blocked using Spamhaus"); len(got) != 1 ||
		got[0].host != "mx.test" || !strings.Contains(got[0].reply, "Spamhaus") {
		t.Errorf("RCPT-stage policy: %+v", got)
	}
	if got := run("554 5.7.1 Service unavailable; client blocked", "250 ok"); len(got) != 1 {
		t.Errorf("banner policy (before RCPT) not reported: %+v", got)
	}
	if got := run("220 ok", "550 5.1.1 User unknown"); len(got) != 0 {
		t.Errorf("a mailbox verdict reached the stand-down: %+v", got)
	}
	if got := run("220 ok", "450 4.2.0 Greylisted"); len(got) != 0 {
		t.Errorf("a deferral reached the stand-down: %+v", got)
	}
}
