package prober

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Plan 029: the catch-all question is asked only when a 250 needs it, one bogus
// address at a time, stopping at the first rejection.

// bogusMX scripts a server: real addresses are answered by real(addr), and each
// bogus address (a 24-hex local part) by the next entry of bogus, last one
// repeating. It counts the bogus RCPTs it was asked.
type bogusMX struct {
	mu    sync.Mutex
	asked int
	real  func(addr string) string
	bogus []string
}

func (m *bogusMX) dialer() Dialer {
	return scriptedMX("220 ok", func(cmd string) string {
		if !strings.HasPrefix(cmd, "RCPT TO:<") {
			if strings.HasPrefix(cmd, "RSET") {
				return ""
			}
			return "250 ok"
		}
		addr := strings.TrimSuffix(strings.TrimPrefix(cmd, "RCPT TO:<"), ">")
		local, _, _ := strings.Cut(addr, "@")
		if len(local) == 24 && isHex(local) {
			m.mu.Lock()
			defer m.mu.Unlock()
			i := min(m.asked, len(m.bogus)-1)
			m.asked++
			return m.bogus[i]
		}
		return m.real(addr)
	})
}

func (m *bogusMX) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asked
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

const (
	accept = "250 2.1.5 OK"
	reject = "550 5.1.1 user unknown"
)

func acceptAll(string) string { return accept }

// countingRecorder records catch-all outcomes; the other Recorder methods are no-ops.
type countingRecorder struct {
	mu       sync.Mutex
	outcomes map[string]int
}

func (c *countingRecorder) Result(string)     {}
func (c *countingRecorder) Reply(int, string) {}
func (c *countingRecorder) Blocked(string)    {}
func (c *countingRecorder) CatchAllProbes(o string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcomes == nil {
		c.outcomes = map[string]int{}
	}
	c.outcomes[o]++
}

func probeCatchAll(t *testing.T, mx *bogusMX, opts Options, emails ...string) (Response, *countingRecorder) {
	t.Helper()
	rec := &countingRecorder{}
	opts.Dialer, opts.Resolver, opts.Timeout, opts.Metrics = mx.dialer(), stubResolver{}, 5*time.Second, rec
	if opts.Rand == nil {
		opts.Rand = func() float64 { return 0.99 } // no audit unless a test asks
	}
	resp, err := New(opts).Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test", NeedCatchAll: true, Emails: emails,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp, rec
}

func verdictOf(t *testing.T, r Result) (catchAll, randomiser *bool) {
	t.Helper()
	return r.CatchAll, r.Randomiser
}

func is(p *bool, want bool) bool { return p != nil && *p == want }

func TestCleanHostIsSettledByOneBogusRCPT(t *testing.T) {
	mx := &bogusMX{real: acceptAll, bogus: []string{reject}}
	resp, rec := probeCatchAll(t, mx, Options{}, "a@example.test")
	ca, rnd := verdictOf(t, resp.Results["a@example.test"])
	if mx.count() != 1 || !is(ca, false) || !is(rnd, false) {
		t.Fatalf("bogus=%d catch_all=%v randomiser=%v, want 1 / false / false", mx.count(), ca, rnd)
	}
	if rec.outcomes[catchAllCleanAfter1] != 1 {
		t.Errorf("outcomes %v", rec.outcomes)
	}
}

func TestCatchAllHostTakesTwo(t *testing.T) {
	mx := &bogusMX{real: acceptAll, bogus: []string{accept}}
	resp, rec := probeCatchAll(t, mx, Options{}, "a@example.test")
	ca, rnd := verdictOf(t, resp.Results["a@example.test"])
	if mx.count() != 2 || !is(ca, true) || !is(rnd, false) {
		t.Fatalf("bogus=%d catch_all=%v randomiser=%v, want 2 / true / false", mx.count(), ca, rnd)
	}
	if rec.outcomes[catchAllCatchAll] != 1 {
		t.Errorf("outcomes %v", rec.outcomes)
	}
}

func TestRandomiserTakesTwoAndIsRemembered(t *testing.T) {
	mx := &bogusMX{real: acceptAll, bogus: []string{accept, reject}}
	profiles := newProfiles()
	resp, rec := probeCatchAll(t, mx, Options{Profiles: profiles}, "a@example.test")
	ca, rnd := verdictOf(t, resp.Results["a@example.test"])
	if mx.count() != 2 || !is(ca, true) || !is(rnd, true) {
		t.Fatalf("bogus=%d catch_all=%v randomiser=%v, want 2 / true / true", mx.count(), ca, rnd)
	}
	if !profiles.isMarked("mx.test") {
		t.Error("randomising host not remembered")
	}
	if rec.outcomes[catchAllRandomiser] != 1 {
		t.Errorf("outcomes %v", rec.outcomes)
	}
}

func TestNoAcceptedAddressMeansNoBogusRCPT(t *testing.T) {
	for name, reply := range map[string]string{
		"all 5.1.1": reject,
		"all 5.4.1": "550 5.4.1 Recipient address rejected: Access denied",
		"greylist":  "450 4.2.0 Greylisted, try again later",
	} {
		t.Run(name, func(t *testing.T) {
			mx := &bogusMX{real: func(string) string { return reply }, bogus: []string{accept}}
			resp, rec := probeCatchAll(t, mx, Options{}, "a@example.test", "b@example.test")
			if mx.count() != 0 {
				t.Fatalf("asked %d bogus RCPTs with no 250 to qualify", mx.count())
			}
			for addr, r := range resp.Results {
				if r.CatchAll != nil || r.Randomiser != nil {
					t.Errorf("%s: catch_all=%v randomiser=%v, want both nil", addr, r.CatchAll, r.Randomiser)
				}
			}
			if rec.outcomes[catchAllSkippedNoAccept] != 1 {
				t.Errorf("outcomes %v", rec.outcomes)
			}
		})
	}
}

func TestTheVerdictIsEstablishedInTheFirstChunkWithA250(t *testing.T) {
	// Chunks of 2: the first chunk's addresses are rejected, the second's accepted.
	answer := func(addr string) string {
		if strings.HasPrefix(addr, "late") {
			return accept
		}
		return reject
	}
	mx := &bogusMX{real: answer, bogus: []string{reject}}
	resp, _ := probeCatchAll(t, mx, Options{MaxRCPTPerSession: 2},
		"early1@example.test", "early2@example.test", "late1@example.test")
	if mx.count() != 1 {
		t.Fatalf("bogus=%d, want 1 (asked in the second chunk)", mx.count())
	}
	late := resp.Results["late1@example.test"]
	if !is(late.CatchAll, false) || !is(late.Randomiser, false) {
		t.Fatalf("late 250 got catch_all=%v randomiser=%v, want false/false", late.CatchAll, late.Randomiser)
	}
}

func TestAnAuditSampleAsksTheFullSequence(t *testing.T) {
	mx := &bogusMX{real: acceptAll, bogus: []string{reject}}
	_, rec := probeCatchAll(t, mx, Options{CatchAllAuditRate: 0.05, Rand: func() float64 { return 0.01 }}, "a@example.test")
	if mx.count() != auditProbes {
		t.Fatalf("audit asked %d, want %d regardless of the first rejection", mx.count(), auditProbes)
	}
	if rec.outcomes[catchAllAuditFull] != 1 {
		t.Errorf("outcomes %v", rec.outcomes)
	}
}

func TestEveryBogusRCPTTakesAToken(t *testing.T) {
	mx := &bogusMX{real: acceptAll, bogus: []string{accept}}
	pc := &recordingPacer{}
	var paced int
	probeCatchAll(t, mx, Options{Pacer: pc, OnPaced: func(string) { paced++ }}, "a@example.test", "b@example.test")
	// 2 real + 2 bogus, one token each.
	if pc.acquires != 4 || paced != 4 {
		t.Fatalf("acquires=%d paced=%d, want 4 each", pc.acquires, paced)
	}
}

func TestNoBudgetMeansNoBogusRCPT(t *testing.T) {
	// The real RCPT's token is granted, every later one refused (invariant 5).
	mx := &bogusMX{real: acceptAll, bogus: []string{accept}}
	pc := &budgetAfter{grant: 1}
	resp, _ := probeCatchAll(t, mx, Options{Pacer: pc}, "a@example.test")
	if mx.count() != 0 {
		t.Fatalf("asked %d bogus RCPTs without budget", mx.count())
	}
	if r := resp.Results["a@example.test"]; r.Class != ClassValid || r.CatchAll != nil {
		t.Errorf("real result %+v, want valid with no verdict", r)
	}
}

func TestABogusAnswerNeverChangesARealResult(t *testing.T) {
	mx := &bogusMX{real: func(addr string) string {
		if strings.HasPrefix(addr, "gone") {
			return reject
		}
		return accept
	}, bogus: []string{accept}}
	resp, _ := probeCatchAll(t, mx, Options{}, "here@example.test", "gone@example.test")
	if c := resp.Results["here@example.test"].Class; c != ClassValid {
		t.Errorf("here: %s, want valid", c)
	}
	if c := resp.Results["gone@example.test"].Class; c != ClassInvalid {
		t.Errorf("gone: %s, want invalid", c)
	}
}

// budgetAfter grants the first n tokens and refuses the rest.
type budgetAfter struct {
	mu    sync.Mutex
	grant int
}

func (b *budgetAfter) Acquire(context.Context, string, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.grant > 0 {
		b.grant--
		return nil
	}
	return errors.New("no budget")
}

func (b *budgetAfter) Observe(context.Context, string, bool) {}
