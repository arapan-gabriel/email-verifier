package prober

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// transactionMX is a Dialer backed by an in-memory pipe that answers every
// command, RSET included — a grouped session (plan 033) reads the RSET that
// opens each further domain's transaction. Replies are queued and written by
// their own goroutine, so the teardown's unread RSET/QUIT answers cannot block
// the pipe. reply returns the line(s) to send and whether to drop the
// connection instead. It also counts the connections opened and records
// every command.
type transactionMX struct {
	mu    sync.Mutex
	conns int
	cmds  []string
	reply func(conn int, cmd string) (out string, drop bool)
}

func (m *transactionMX) DialContext(context.Context, string, string) (net.Conn, error) {
	m.mu.Lock()
	m.conns++
	n := m.conns
	m.mu.Unlock()
	client, server := net.Pipe()
	queue := make(chan string, 64)
	go func() {
		for line := range queue {
			if _, err := fmt.Fprintf(server, "%s\r\n", line); err != nil {
				break
			}
		}
		// Closing ends the reader too; the queue's buffer outlasts it.
		_ = server.Close()
	}()
	go func() {
		defer close(queue)
		queue <- "220 mx.test ESMTP"
		br := bufio.NewReader(server)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimRight(line, "\r\n")
			m.mu.Lock()
			m.cmds = append(m.cmds, cmd)
			m.mu.Unlock()
			if strings.HasPrefix(cmd, "QUIT") {
				return
			}
			out, drop := m.reply(n, cmd)
			if drop {
				_ = server.Close()
				return
			}
			queue <- out
		}
	}()
	return client, nil
}

// workspaceReply answers like an honest one-system-many-domains MX:
// valid@ exists, everything else is 550 5.1.1.
func workspaceReply(cmd string) string {
	switch {
	case strings.HasPrefix(cmd, "EHLO"):
		return "250 mx.test"
	case strings.HasPrefix(cmd, "RCPT TO:<valid@"):
		return "250 2.1.5 OK"
	case strings.HasPrefix(cmd, "RCPT"):
		return "550 5.1.1 The email account that you tried to reach does not exist"
	}
	return "250 2.0.0 OK"
}

type fallbackSeen struct{ family, reason string }

func groupedProber(d Dialer, pc Pacer, seen *[]fallbackSeen) *Prober {
	return New(Options{
		Dialer: d, Resolver: stubResolver{}, Timeout: 5 * time.Second,
		Helo: "mail.test", MailFrom: "verify@probe.test", Pacer: pc,
		MultiDomain: func(string) (string, bool) { return "@google", true },
		OnFallback: func(family, _, reason, _ string) {
			*seen = append(*seen, fallbackSeen{family, reason})
		},
	})
}

func threeGroups() Request {
	return Request{MXHost: "aspmx.l.google.com", Domains: []DomainGroup{
		{Domain: "a.test", Emails: []string{"valid@a.test", "nope@a.test"}},
		{Domain: "b.test", Emails: []string{"valid@b.test"}},
		{Domain: "c.test", Emails: []string{"valid@c.test"}},
	}}
}

// A connection that dies between transactions leaves the domains not yet asked
// unattempted — never a verdict — and is not a refusal of grouping.
func TestAConnectionLostBetweenTransactionsLeavesTheRestUnattempted(t *testing.T) {
	mx := &transactionMX{reply: func(_ int, cmd string) (string, bool) {
		return workspaceReply(cmd), cmd == "RSET"
	}}
	var seen []fallbackSeen
	resp, err := groupedProber(mx, nil, &seen).Probe(t.Context(), threeGroups())
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Results["valid@a.test"]; got.Class != ClassValid {
		t.Errorf("first domain = %s, want its answer kept", got.Class)
	}
	if got := resp.Results["nope@a.test"]; got.Class != ClassInvalid {
		t.Errorf("first domain's bounce = %s, want invalid", got.Class)
	}
	for _, a := range []string{"valid@b.test", "valid@c.test"} {
		got := resp.Results[a]
		if got.Accepted != nil || got.Connected == nil || *got.Connected || !got.Class.IsTemp() {
			t.Errorf("%s = %+v, want unattempted with no verdict", a, got)
		}
	}
	if len(seen) != 0 || mx.conns != 1 {
		t.Errorf("fallbacks %v, connections %d — a dropped session is no refusal of grouping", seen, mx.conns)
	}
}

// A throttle answering the next transaction's MAIL FROM is a rate signal about
// the session, not a refusal of grouping: the rest are unattempted with a
// retry hint, and the pacer hears it.
func TestAThrottleAtTheNextMailFromIsNotAGroupingRefusal(t *testing.T) {
	mails := 0
	mx := &transactionMX{reply: func(_ int, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "MAIL FROM") {
			mails++
			if mails > 1 {
				return "421 4.7.0 Try again later, closing connection", false
			}
		}
		return workspaceReply(cmd), false
	}}
	pc := &recordingPacer{}
	var seen []fallbackSeen
	resp, err := groupedProber(mx, pc, &seen).Probe(t.Context(), threeGroups())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"valid@b.test", "valid@c.test"} {
		got := resp.Results[a]
		if got.Class != ClassThrottled || got.Accepted != nil || got.RetryAfterSeconds == 0 {
			t.Errorf("%s = %+v, want throttled, unattempted, with a retry hint", a, got)
		}
	}
	if pc.throttles() != 1 || len(seen) != 0 {
		t.Errorf("throttles %d, fallbacks %v — want 1 and none", pc.throttles(), seen)
	}
}

// A receiver that refuses a second domain per *connection* — the same 451 as
// Google's per-transaction refusal, RSET or not — still gets every address
// answered: the refused one and the rest are asked again one domain per
// session, inside the same request, and grouping is off for the family.
func TestADomainLimitPerConnectionIsReaskedAlone(t *testing.T) {
	domains := map[int]string{}
	var mu sync.Mutex
	mx := &transactionMX{reply: func(conn int, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "RCPT TO:<") {
			_, d, _ := strings.Cut(strings.TrimSuffix(cmd, ">"), "@")
			mu.Lock()
			defer mu.Unlock()
			if first, ok := domains[conn]; ok && first != d {
				return "451-4.3.0 Multiple destination domains per transaction is unsupported. Please\r\n" +
					"451 4.3.0 try again. x - gsmtp", false
			}
			domains[conn] = d
		}
		return workspaceReply(cmd), false
	}}
	var seen []fallbackSeen
	p := groupedProber(mx, nil, &seen)
	resp, err := p.Probe(t.Context(), threeGroups())
	if err != nil {
		t.Fatal(err)
	}
	for a, want := range map[string]Class{
		"valid@a.test": ClassValid, "nope@a.test": ClassInvalid,
		"valid@b.test": ClassValid, "valid@c.test": ClassValid,
	} {
		if got := resp.Results[a]; got.Class != want {
			t.Errorf("%s = %s (%q), want %s", a, got.Class, got.Reply, want)
		}
	}
	if len(seen) != 1 || seen[0] != (fallbackSeen{"@google", FallbackDomainLimit}) {
		t.Errorf("fallbacks %v, want one @google domain_limit", seen)
	}
	// The grouped session, then b and c alone.
	if mx.conns != 3 {
		t.Errorf("connections = %d, want 3", mx.conns)
	}

	// From now on one domain per session: no second domain is ever offered.
	before := mx.conns
	if _, err := p.Probe(t.Context(), threeGroups()); err != nil {
		t.Fatal(err)
	}
	if got := mx.conns - before; got != 3 || len(seen) != 1 {
		t.Errorf("after the fallback: %d connections, %d fallbacks — want 3 and still 1", got, len(seen))
	}
}

// Each further domain opens its own transaction: RSET, then MAIL FROM with the
// same sender, before its first RCPT.
func TestEachDomainGetsItsOwnTransaction(t *testing.T) {
	mx := &transactionMX{reply: func(_ int, cmd string) (string, bool) { return workspaceReply(cmd), false }}
	var seen []fallbackSeen
	if _, err := groupedProber(mx, nil, &seen).Probe(t.Context(), threeGroups()); err != nil {
		t.Fatal(err)
	}
	mx.mu.Lock()
	var got []string
	for _, c := range mx.cmds {
		if !strings.HasPrefix(c, "EHLO") {
			got = append(got, c)
		}
	}
	mx.mu.Unlock()
	want := []string{
		"MAIL FROM:<verify@probe.test>", "RCPT TO:<valid@a.test>", "RCPT TO:<nope@a.test>",
		"RSET", "MAIL FROM:<verify@probe.test>", "RCPT TO:<valid@b.test>",
		"RSET", "MAIL FROM:<verify@probe.test>", "RCPT TO:<valid@c.test>",
		"RSET", "QUIT",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("dialogue\n got %v\nwant %v", got, want)
	}
}

func TestDomainLimitRefusal(t *testing.T) {
	for _, tc := range []struct {
		code int
		text string
		want bool
	}{
		{451, "451-4.3.0 Multiple destination domains per transaction is unsupported. Please\n451 4.3.0 try again.", true},
		{451, "451 4.3.0 Try again later", true},
		{452, "452 Too many domains in this session", true},
		{550, "550 5.5.3 Too many destination domains", true},
		{451, "451 4.7.1 Greylisted, try again", false},
		{550, "550 5.3.0 Mailbox unavailable", false},
		{250, "250 2.1.5 OK multiple destination domains welcome", false},
	} {
		if got := domainLimitRefusal(tc.code, tc.text); got != tc.want {
			t.Errorf("domainLimitRefusal(%d, %q) = %v, want %v", tc.code, tc.text, got, tc.want)
		}
	}
}
