package pacer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
)

type fakeStore struct {
	mu sync.Mutex
	kv map[string]string
}

func newStore(seed map[string]string) *fakeStore {
	kv := map[string]string{}
	maps.Copy(kv, seed)
	return &fakeStore{kv: kv}
}

func (f *fakeStore) Get(_ context.Context, k string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[k]
	return v, ok, nil
}

func (f *fakeStore) Set(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[k] = v
	return nil
}

func (f *fakeStore) get(k string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kv[k]
}

// fakeTaker records the rate it was asked to enforce, which is what the pacer
// is actually deciding.
type fakeTaker struct {
	mu     sync.Mutex
	rates  []float64
	refuse int
	err    error
}

func (f *fakeTaker) Take(_ context.Context, _ string, rate, _ float64) (limiter.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return limiter.Decision{}, f.err
	}
	f.rates = append(f.rates, rate)
	if f.refuse > 0 {
		f.refuse--
		return limiter.Decision{Allowed: false, RetryAfter: time.Second}, nil
	}
	return limiter.Decision{Allowed: true}, nil
}

func (f *fakeTaker) lastRate() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rates) == 0 {
		return 0
	}
	return f.rates[len(f.rates)-1]
}

// band is a small, exactly-known range so the arithmetic is readable.
const testBand = `{"min_rate_per_sec":0.5,"max_rate_per_sec":4,"min_concurrency":1,
	"max_concurrency":4,"burst":2,"cooldown_seconds":60,"pause_seconds":300}`

func newPacer(t *testing.T, seed map[string]string) (*Pacer, *fakeStore, *fakeTaker) {
	t.Helper()
	kv := map[string]string{"limits:mx:mx.test": testBand}
	maps.Copy(kv, seed)
	s, tk := newStore(kv), &fakeTaker{}
	return New(s, tk, Options{}), s, tk
}

func TestStartsAtTheCeiling(t *testing.T) {
	p, _, tk := newPacer(t, nil)
	if err := p.Acquire(t.Context(), "mx.test", "example.test"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := tk.lastRate(); got != 4 {
		t.Errorf("started at %v, want the band ceiling 4", got)
	}
	if s := p.State("mx.test"); s != StateProbing {
		t.Errorf("state = %s, want %s", s, StateProbing)
	}
}

// The AIMD loop: halve on a real throttle, never below the floor.
func TestThrottleHalvesDownToTheFloor(t *testing.T) {
	p, _, tk := newPacer(t, nil)
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []float64{2, 1, 0.5, 0.5} {
		p.Observe(ctx, "mx.test", true)
		got, _ := p.Rate("mx.test")
		if got != want {
			t.Fatalf("after a throttle rate = %v, want %v", got, want)
		}
	}
	if s := p.State("mx.test"); s != StateBackoff && s != StatePaused {
		t.Errorf("state = %s", s)
	}
	_ = tk
}

func TestClimbsOnlyAfterTenCleanAnswers(t *testing.T) {
	p, _, _ := newPacer(t, nil)
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx, "mx.test", true) // 4 -> 2

	for i := range 9 {
		p.Observe(ctx, "mx.test", false)
		if got, _ := p.Rate("mx.test"); got != 2 {
			t.Fatalf("rate moved after %d clean answers: %v", i+1, got)
		}
	}
	p.Observe(ctx, "mx.test", false) // the tenth
	if got, _ := p.Rate("mx.test"); got != 2*climbFactor {
		t.Errorf("rate = %v, want %v after ten clean answers", got, 2*climbFactor)
	}
}

func TestClimbStopsAtTheCeiling(t *testing.T) {
	p, _, _ := newPacer(t, nil)
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		p.Observe(ctx, "mx.test", false)
	}
	if got, _ := p.Rate("mx.test"); got != 4 {
		t.Errorf("rate = %v, want the ceiling 4 — a band is never exceeded", got)
	}
}

// At the floor and still throttled, the MX is stood down rather than poked.
// synctest runs the five-minute cooldown in microseconds.
func TestPausesAtTheFloorAndResumesAfterCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, store, _ := newPacer(t, nil)
		ctx := context.Background()
		if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
			t.Fatal(err)
		}
		for range 4 { // 4 -> 2 -> 1 -> 0.5 -> paused
			p.Observe(ctx, "mx.test", true)
		}
		if s := p.State("mx.test"); s != StatePaused {
			t.Fatalf("state = %s, want %s", s, StatePaused)
		}

		err := p.Acquire(ctx, "mx.test", "example.test")
		if !errors.Is(err, ErrPaused) {
			t.Fatalf("Acquire during the pause = %v, want ErrPaused", err)
		}
		if store.get("rt:mx:mx.test:pause_until") == "" {
			t.Error("the pause was not persisted; a restart would resume probing immediately")
		}

		time.Sleep(301 * time.Second) // pause_seconds is 300
		if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
			t.Errorf("still paused after the cooldown: %v", err)
		}
	})
}

// A refused take is waited out, not failed. synctest makes the wait free.
func TestAcquireWaitsForTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, tk := newStore(map[string]string{"limits:mx:mx.test": testBand}), &fakeTaker{refuse: 3}
		p := New(s, tk, Options{})
		start := time.Now()
		if err := p.Acquire(context.Background(), "mx.test", "example.test"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if waited := time.Since(start); waited != 3*time.Second {
			t.Errorf("waited %s, want 3s — one second per refusal", waited)
		}
	})
}

// Invariant 5. An unconfirmed verdict is recoverable; a blocklist entry is not.
func TestAcquireFailsClosedWhenTheBucketIsUnreachable(t *testing.T) {
	s := newStore(map[string]string{"limits:mx:mx.test": testBand})
	down := errors.New("connection refused")
	p := New(s, &fakeTaker{err: down}, Options{})
	err := p.Acquire(t.Context(), "mx.test", "example.test")
	if !errors.Is(err, down) {
		t.Fatalf("Acquire = %v, want the store failure — pacing must never fail open", err)
	}
}

// Backing off is a measurement; a quiet hour below the ceiling is not evidence
// that the ceiling moved.
func TestSavedRateMayOnlyLowerTheStart(t *testing.T) {
	t.Run("lower is honoured", func(t *testing.T) {
		p, _, tk := newPacer(t, map[string]string{"rt:mx:mx.test:rate": "1"})
		if err := p.Acquire(t.Context(), "mx.test", "example.test"); err != nil {
			t.Fatal(err)
		}
		if got := tk.lastRate(); got != 1 {
			t.Errorf("started at %v, want the saved 1", got)
		}
	})
	t.Run("higher is ignored", func(t *testing.T) {
		p, _, tk := newPacer(t, map[string]string{"rt:mx:mx.test:rate": "99"})
		if err := p.Acquire(t.Context(), "mx.test", "example.test"); err != nil {
			t.Fatal(err)
		}
		if got := tk.lastRate(); got != 4 {
			t.Errorf("started at %v; a saved rate must never raise the ceiling", got)
		}
	})
}

func TestPersistsTheWorkingPoint(t *testing.T) {
	p, store, _ := newPacer(t, nil)
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx, "mx.test", true)

	if got := store.get("rt:mx:mx.test:rate"); got != "2" {
		t.Errorf("persisted rate = %q, want 2", got)
	}
	if got := store.get("rt:mx:mx.test:state"); got != StateBackoff {
		t.Errorf("persisted state = %q, want %s", got, StateBackoff)
	}
	if _, err := strconv.Atoi(store.get("rt:mx:mx.test:conc")); err != nil {
		t.Errorf("persisted conc = %q", store.get("rt:mx:mx.test:conc"))
	}
}

func TestBandResolution(t *testing.T) {
	t.Run("shipped seed for a known provider", func(t *testing.T) {
		b, ok := seedFor("gmail.com")
		if !ok {
			t.Fatal("no shipped band for gmail.com")
		}
		if b.MaxRate <= 0 || b.MinRate > b.MaxRate {
			t.Errorf("band = %s", b)
		}
	})
	t.Run("falls back to the parent domain", func(t *testing.T) {
		if _, ok := seedFor("mail.gmail.com"); !ok {
			t.Error("a sub-domain did not fall back to its parent")
		}
	})
	t.Run("unknown domain gets the conservative default", func(t *testing.T) {
		p, _, tk := newPacer(t, map[string]string{})
		delete(p.store.(*fakeStore).kv, "limits:mx:mx.test")
		if err := p.Acquire(t.Context(), "mx.test", "nobody-has-heard-of-this.test"); err != nil {
			t.Fatal(err)
		}
		if got, want := tk.lastRate(), conservative().MaxRate; got != want {
			t.Errorf("started at %v, want the conservative %v", got, want)
		}
	})
	t.Run("seeds actually shipped", func(t *testing.T) {
		if n := SeedCount(); n < 50 {
			t.Errorf("SeedCount = %d; the bands did not make it into the binary", n)
		}
	})
}

// The map is keyed by a value that arrives in the request. Without a bound a
// bulk run over ten thousand domains holds ten thousand entries for the life of
// the process — and every per-MX metric labelled from it becomes a time series
// that never goes away.
func TestTrackedMXIsBounded(t *testing.T) {
	s, tk := newStore(nil), &fakeTaker{}
	p := New(s, tk, Options{MaxTracked: 32})
	for i := range 500 {
		if err := p.Acquire(t.Context(), fmt.Sprintf("mx%03d.test", i), "example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.Tracked(); got > 32 {
		t.Errorf("tracking %d MXes, cap is 32", got)
	}
	if len(p.Snapshot()) != p.Tracked() {
		t.Error("Snapshot disagrees with Tracked")
	}
}

// Eviction is lossless: the working point is in Redis, so an evicted entry
// costs one re-read rather than a reset to the ceiling.
func TestEvictedStateComesBackFromRedis(t *testing.T) {
	s, tk := newStore(map[string]string{"limits:mx:mx.test": testBand}), &fakeTaker{}
	p := New(s, tk, Options{MaxTracked: 2})
	ctx := t.Context()

	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx, "mx.test", true) // 4 -> 2, persisted

	// Push it out.
	for i := range 10 {
		if err := p.Acquire(ctx, fmt.Sprintf("other%d.test", i), "example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := p.Rate("mx.test"); ok {
		t.Fatal("mx.test survived eviction; the test proves nothing")
	}

	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	if got := tk.lastRate(); got != 2 {
		t.Errorf("resumed at %v, want the persisted 2 — eviction must not reset to the ceiling", got)
	}
}

func TestIdleEntriesAreEvicted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, tk := newStore(nil), &fakeTaker{}
		p := New(s, tk, Options{IdleTTL: 10 * time.Minute, MaxTracked: 1000})
		ctx := context.Background()
		if err := p.Acquire(ctx, "idle.test", "example.test"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Minute)
		// Eviction is opportunistic, so touching another host triggers it.
		if err := p.Acquire(ctx, "fresh.test", "example.test"); err != nil {
			t.Fatal(err)
		}
		if _, ok := p.Rate("idle.test"); ok {
			t.Error("an entry idle past the TTL is still held")
		}
	})
}

type countingRecorder struct{ pauses int }

func (c *countingRecorder) Pause(string) { c.pauses++ }

func TestPauseIsCounted(t *testing.T) {
	rec := &countingRecorder{}
	s := newStore(map[string]string{"limits:mx:mx.test": testBand})
	p := New(s, &fakeTaker{}, Options{Metrics: rec})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 4 { // 4 -> 2 -> 1 -> 0.5 -> paused
		p.Observe(ctx, "mx.test", true)
	}
	if rec.pauses != 1 {
		t.Errorf("counted %d pause events, want 1", rec.pauses)
	}
}

// AIMD can never climb past its own ceiling, and every shipped band says
// "confidence": "guess". Without this, an MX that tolerates more than its seed
// says sits under-used forever with nothing noticing.
func TestCleanAnswersAtTheCeilingProposeAWiderBand(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand}) // ceiling 4
	p := New(store, &fakeTaker{}, Options{Promote: Promotion{After: 10, Step: 1.5, Ceiling: 20}})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	// Starts at the ceiling, so every clean answer is evidence.
	for range 10 {
		p.Observe(ctx, "mx.test", false)
	}
	pr, ok := p.Proposal(ctx, "mx.test")
	if !ok {
		t.Fatal("no proposal after ten clean answers at the ceiling")
	}
	if pr.CurrentMax != 4 || pr.ProposedMax != 6 {
		t.Errorf("proposal = %v -> %v, want 4 -> 6", pr.CurrentMax, pr.ProposedMax)
	}
	// The evidence is recorded, not just the conclusion.
	if pr.CleanAt < 10 || pr.FormedAt == 0 {
		t.Errorf("proposal carries no usable evidence: %+v", pr)
	}
	// And crucially: nothing was applied.
	if rate, _ := p.Rate("mx.test"); rate != 4 {
		t.Errorf("rate = %v; a proposal must not raise anything by itself", rate)
	}
}

// Answering cleanly below the ceiling says nothing about whether the ceiling is
// the limit.
func TestCleanAnswersBelowTheCeilingAreNotEvidence(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand})
	p := New(store, &fakeTaker{}, Options{Promote: Promotion{After: 5}})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx, "mx.test", true) // 4 -> 2, now below the ceiling
	for range 20 {
		p.Observe(ctx, "mx.test", false)
	}
	if _, ok := p.Proposal(ctx, "mx.test"); ok {
		t.Error("clean answers below the ceiling produced a proposal")
	}
}

func TestOneThrottleResetsTheEvidence(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand})
	p := New(store, &fakeTaker{}, Options{Promote: Promotion{After: 10}})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 9 {
		p.Observe(ctx, "mx.test", false)
	}
	p.Observe(ctx, "mx.test", true) // the ceiling is not proven after all
	for range 9 {
		p.Observe(ctx, "mx.test", false)
	}
	if _, ok := p.Proposal(ctx, "mx.test"); ok {
		t.Error("a proposal survived a real throttle")
	}
}

// No run of clean answers may propose a rate nobody sanctioned.
func TestProposalIsCappedInAbsoluteTerms(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand})
	p := New(store, &fakeTaker{}, Options{Promote: Promotion{After: 2, Step: 100, Ceiling: 5}})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		p.Observe(ctx, "mx.test", false)
	}
	pr, ok := p.Proposal(ctx, "mx.test")
	if !ok {
		t.Fatal("no proposal")
	}
	if pr.ProposedMax != 5 {
		t.Errorf("proposed %v, want the absolute ceiling 5", pr.ProposedMax)
	}
}

func TestPromoteAppliesAndClears(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand})
	tk := &fakeTaker{}
	p := New(store, tk, Options{Promote: Promotion{After: 5, Step: 2, Ceiling: 20}})
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		p.Observe(ctx, "mx.test", false)
	}

	applied, err := p.Promote(ctx, "mx.test")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if applied.ProposedMax != 8 {
		t.Errorf("promoted to %v, want 8", applied.ProposedMax)
	}
	if _, ok := p.Proposal(ctx, "mx.test"); ok {
		t.Error("the proposal survived its promotion")
	}
	// The next request re-reads the band; no restart. It does **not** jump to
	// the new ceiling: a saved rate may only ever lower the start, and a
	// ceiling is earned by clean answers rather than granted by config
	// (RUNBOOK). Promotion widens the permission; AIMD still has to reach it.
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	if got := tk.lastRate(); got != 4 {
		t.Errorf("resumed at %v, want the earned 4 — promotion grants permission, not rate", got)
	}
	// But the permission is real: AIMD may now climb past the old ceiling.
	for range 200 {
		p.Observe(ctx, "mx.test", false)
	}
	rate, _ := p.Rate("mx.test")
	if rate <= 4 {
		t.Errorf("rate = %v; after promotion the loop should be able to climb past the old 4", rate)
	}
	if rate > 8 {
		t.Errorf("rate = %v; it must not exceed the promoted ceiling 8", rate)
	}
}

func TestPromoteWithoutAProposalIsAnError(t *testing.T) {
	p := New(newStore(nil), &fakeTaker{}, Options{})
	if _, err := p.Promote(t.Context(), "mx.test"); err == nil {
		t.Error("promoted a band with no standing proposal")
	}
}

func TestProposalsDisabledByDefault(t *testing.T) {
	store := newStore(map[string]string{"limits:mx:mx.test": testBand})
	p := New(store, &fakeTaker{}, Options{}) // After == 0
	ctx := t.Context()
	if err := p.Acquire(ctx, "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		p.Observe(ctx, "mx.test", false)
	}
	if _, ok := p.Proposal(ctx, "mx.test"); ok {
		t.Error("a proposal appeared with promotion disabled")
	}
}
