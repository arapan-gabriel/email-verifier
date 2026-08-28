package smtp_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/smtp"
)

// lab spins up one profile on an ephemeral port with a fast-forwardable clock.
type lab struct {
	eng  *policy.Engine
	clk  *clock.Offsetting
	addr string
}

func newLab(t *testing.T, p *policy.Profile) *lab {
	t.Helper()
	p.ApplyDefaults()
	clk := clock.New()
	eng := policy.NewEngine(p, clk)
	srv := smtp.New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
	return &lab{eng: eng, clk: clk, addr: ln.Addr().String()}
}

// greeting dials and returns just the first line the server sends. During a
// cooldown the greeting itself is the 421, so a client that assumes a banner
// will mis-read the refusal.
func (l *lab) greeting(t *testing.T) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", l.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (l *lab) dial(t *testing.T) *client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", l.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.read() // banner
	return c
}

// read consumes one (possibly multiline) SMTP reply and returns the last line.
func (c *client) read() string {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) >= 4 && line[3] == '-' {
			continue // continuation
		}
		return line
	}
}

func (c *client) cmd(s string) string {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, s+"\r\n"); err != nil {
		c.t.Fatalf("write %q: %v", s, err)
	}
	return c.read()
}

// cmdExpectFailure expects the connection to fail rather than answer.
func (c *client) cmdExpectFailure(s string, within time.Duration) error {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, s+"\r\n"); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(within))
	_, err := c.r.ReadString('\n')
	return err
}

func (c *client) hello() { c.cmd("EHLO probe.test") }

func (c *client) mail() { c.cmd("MAIL FROM:<probe@ds-testing.test>") }

func code(s string) string {
	if len(s) < 3 {
		return ""
	}
	return s[:3]
}

func mustCode(t *testing.T, got, want, what string) {
	t.Helper()
	if code(got) != want {
		t.Fatalf("%s: want %s, got %q", what, want, got)
	}
}

func base(name string) *policy.Profile {
	return &policy.Profile{
		Name:    name,
		Domains: []string{name + "-sim.test"},
		Listen:  []string{"127.0.0.1:0"},
		Recipients: policy.Recipients{
			Exists:  []string{"valid@"},
			Bounce:  []string{"nope@"},
			Timeout: []string{"blackhole@"},
			Drop:    []string{"reset@"},
		},
	}
}

// Scenario 1: the baseline every other test depends on.
func TestAcceptAndReject(t *testing.T) {
	l := newLab(t, base("postfix"))
	c := l.dial(t)
	c.hello()
	c.mail()
	mustCode(t, c.cmd("RCPT TO:<valid@postfix-sim.test>"), "250", "known recipient")
	mustCode(t, c.cmd("RCPT TO:<nope@postfix-sim.test>"), "550", "listed bounce")
	mustCode(t, c.cmd("RCPT TO:<whoever@postfix-sim.test>"), "550", "unknown recipient")
	mustCode(t, c.cmd("QUIT"), "221", "quit")

	st := l.eng.Stats()
	if st.Accepted != 1 || st.Rejected != 2 || st.Rcpt != 3 {
		t.Fatalf("stats: %+v", st)
	}
}

// Scenario 9: a 503 must only ever be our own client's bug, never a block.
func TestBadSequenceIs503(t *testing.T) {
	l := newLab(t, base("postfix"))

	c := l.dial(t)
	c.hello()
	mustCode(t, c.cmd("RCPT TO:<valid@postfix-sim.test>"), "503", "RCPT before MAIL")

	c2 := l.dial(t)
	mustCode(t, c2.cmd("MAIL FROM:<probe@x.test>"), "503", "MAIL before EHLO")

	if got := l.eng.Stats().BadSequence; got != 2 {
		t.Fatalf("bad_sequence: want 2, got %d", got)
	}
}

// Scenario 3 + 10: exceeding the RCPT rate throttles, starts a cooldown that
// refuses new connections, and expires on its own.
func TestRcptRateThrottleThenCooldownExpires(t *testing.T) {
	p := base("gmail")
	p.Limits.RcptRate = policy.RateRule{Count: 3, Window: policy.Duration(time.Minute),
		OnExceed: "421 4.7.0 Too many requests"}
	p.Limits.CooldownAfterExceed = policy.Duration(5 * time.Minute)
	l := newLab(t, p)

	c := l.dial(t)
	c.hello()
	c.mail()
	for i := 0; i < 3; i++ {
		mustCode(t, c.cmd("RCPT TO:<valid@gmail-sim.test>"), "250", "within limit")
	}
	mustCode(t, c.cmd("RCPT TO:<valid@gmail-sim.test>"), "421", "over limit")

	// The 421 closes the connection, and the cooldown then refuses the next
	// one at the banner. That is what makes a naive "just reconnect" retry
	// loop dig the hole deeper.
	mustCode(t, l.greeting(t), "421", "connection during cooldown")

	st := l.eng.Stats()
	if st.Throttled == 0 || st.Cooldowns == 0 {
		t.Fatalf("expected throttle + cooldown, got %+v", st)
	}
	if st.FirstThrottleAt == nil {
		t.Fatal("first_throttle_at not recorded")
	}

	// Fast-forward past the cooldown instead of sleeping five minutes.
	l.clk.Advance(6 * time.Minute)
	c3 := l.dial(t)
	c3.hello()
	c3.mail()
	mustCode(t, c3.cmd("RCPT TO:<valid@gmail-sim.test>"), "250", "after cooldown expiry")
}

func TestMaxConcurrentConns(t *testing.T) {
	p := base("gmail")
	p.Limits.MaxConcurrentConns = 2
	p.Limits.CooldownAfterExceed = 0 // isolate the concurrency cap
	l := newLab(t, p)

	a, b := l.dial(t), l.dial(t)
	a.hello()
	b.hello()

	mustCode(t, l.greeting(t), "421", "third concurrent connection")
	if got := l.eng.Stats().MaxConcurrentSeen; got != 2 {
		t.Fatalf("max_concurrent_seen: want 2, got %d", got)
	}
}

// Scenario 5: greylisting must resolve to a real answer on retry, not to
// "unknown".
func TestGreylistResolvesOnRetry(t *testing.T) {
	p := base("yahoo")
	p.Behaviour.Greylist = true
	p.Behaviour.GreylistDelay = policy.Duration(5 * time.Minute)
	l := newLab(t, p)

	c := l.dial(t)
	c.hello()
	c.mail()
	mustCode(t, c.cmd("RCPT TO:<valid@yahoo-sim.test>"), "450", "first sighting")

	// Retrying too early stays greylisted.
	l.clk.Advance(1 * time.Minute)
	mustCode(t, c.cmd("RCPT TO:<valid@yahoo-sim.test>"), "450", "retry before delay")

	l.clk.Advance(5 * time.Minute)
	mustCode(t, c.cmd("RCPT TO:<valid@yahoo-sim.test>"), "250", "retry after delay")
	mustCode(t, c.cmd("RCPT TO:<nope@yahoo-sim.test>"), "450", "new tuple is greylisted too")
}

// Scenario 6: a catch-all answers 250 for nonsense, so 250 alone never means
// "valid".
func TestCatchAllAcceptsAnything(t *testing.T) {
	p := base("catchall")
	p.Behaviour.CatchAll = true
	p.Recipients = policy.Recipients{}
	l := newLab(t, p)

	c := l.dial(t)
	c.hello()
	c.mail()
	for _, addr := range []string{"valid@", "definitely-not-real-9f3a@", "x@"} {
		mustCode(t, c.cmd("RCPT TO:<"+addr+"catchall-sim.test>"), "250", addr)
	}
}

// Scenario 8: a mid-session drop must surface as a connection error, not as a
// reply, and must not take the server down.
func TestRecipientDropClosesConnection(t *testing.T) {
	l := newLab(t, base("postfix"))
	c := l.dial(t)
	c.hello()
	c.mail()
	if err := c.cmdExpectFailure("RCPT TO:<reset@postfix-sim.test>", 5*time.Second); err == nil {
		t.Fatal("expected the connection to drop, got a reply")
	}
	if got := l.eng.Stats().Drops; got != 1 {
		t.Fatalf("drops: want 1, got %d", got)
	}

	// The server still serves everyone else.
	c2 := l.dial(t)
	c2.hello()
	c2.mail()
	mustCode(t, c2.cmd("RCPT TO:<valid@postfix-sim.test>"), "250", "after a drop")
}

// Scenario 7: a black-holed recipient never answers; the client must time out.
func TestRecipientTimeoutHangs(t *testing.T) {
	p := base("postfix")
	p.Behaviour.TimeoutHold = policy.Duration(3 * time.Second)
	l := newLab(t, p)

	c := l.dial(t)
	c.hello()
	c.mail()
	start := time.Now()
	err := c.cmdExpectFailure("RCPT TO:<blackhole@postfix-sim.test>", 700*time.Millisecond)
	if err == nil {
		t.Fatal("expected no reply, got one")
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("expected a client timeout, got %v after %s", err, time.Since(start))
	}
	if got := l.eng.Stats().Timeouts; got != 1 {
		t.Fatalf("timeouts: want 1, got %d", got)
	}
}

func TestRcptPerConnLimit(t *testing.T) {
	p := base("gmail")
	p.Limits.RcptPerConn = 2
	l := newLab(t, p)

	c := l.dial(t)
	c.hello()
	c.mail()
	mustCode(t, c.cmd("RCPT TO:<valid@gmail-sim.test>"), "250", "1st")
	mustCode(t, c.cmd("RCPT TO:<valid@gmail-sim.test>"), "250", "2nd")
	mustCode(t, c.cmd("RCPT TO:<valid@gmail-sim.test>"), "452", "3rd on same connection")
}

func TestChaosTempErrorsAreDeterministic(t *testing.T) {
	run := func() []string {
		p := base("gmail")
		p.Chaos = policy.Chaos{TempErrorRate: 0.5, Seed: 99}
		l := newLab(t, p)
		c := l.dial(t)
		c.hello()
		c.mail()
		var out []string
		for i := 0; i < 12; i++ {
			out = append(out, code(c.cmd("RCPT TO:<valid@gmail-sim.test>")))
		}
		return out
	}
	a, b := run(), run()
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("same seed produced different sequences:\n%v\n%v", a, b)
	}
	var temp int
	for _, c := range a {
		if c == "451" {
			temp++
		}
	}
	if temp == 0 || temp == len(a) {
		t.Fatalf("expected a mix of 250 and 451 at rate 0.5, got %v", a)
	}
}

func TestVrfyNeverConfirms(t *testing.T) {
	l := newLab(t, base("postfix"))
	c := l.dial(t)
	c.hello()
	// Trusting VRFY is a classic validator bug: real MXes answer 252.
	mustCode(t, c.cmd("VRFY valid@postfix-sim.test"), "252", "vrfy")
}

func TestConcurrentSessionsAreIsolated(t *testing.T) {
	p := base("postfix")
	p.Limits.MaxConcurrentConns = 50
	l := newLab(t, p)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := l.dial(t)
			c.hello()
			c.mail()
			c.cmd("RCPT TO:<valid@postfix-sim.test>")
			c.cmd("QUIT")
		}()
	}
	wg.Wait()

	st := l.eng.Stats()
	if st.Rcpt != 20 || st.Accepted != 20 {
		t.Fatalf("stats: %+v", st)
	}
	if st.CurrentConcurrent != 0 {
		t.Fatalf("connections leaked: %d still active", st.CurrentConcurrent)
	}
}
