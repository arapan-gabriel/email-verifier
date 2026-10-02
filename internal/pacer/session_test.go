package pacer

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
)

// fakeLeaser grants after `refuse` refusals, recording every key and limit asked.
type fakeLeaser struct {
	mu       sync.Mutex
	refuse   int
	retry    time.Duration
	err      error
	keys     []string
	limits   []int
	released []string
}

func (f *fakeLeaser) Lease(_ context.Context, key string, limit int, _ time.Duration) (limiter.LeaseDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return limiter.LeaseDecision{}, f.err
	}
	if f.refuse > 0 {
		f.refuse--
		return limiter.LeaseDecision{Held: limit, RetryAfter: f.retry}, nil
	}
	return limiter.LeaseDecision{Granted: true, ID: "lease-1", Held: 1}, nil
}

func (f *fakeLeaser) Release(_ context.Context, key, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, key+"/"+id)
	return nil
}

type leaseCounter struct {
	mu       sync.Mutex
	outcomes map[string]int
}

func (c *leaseCounter) LeaseWait(o string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcomes == nil {
		c.outcomes = map[string]int{}
	}
	c.outcomes[o]++
}

func leasedPacer(l Leaser, rec LeaseRecorder, wait time.Duration) *Pacer {
	return New(newStore(nil), &fakeTaker{}, Options{Leases: l, LeaseWait: wait, LeaseMetrics: rec})
}

func TestASessionLeaseIsTakenAndGivenBack(t *testing.T) {
	l := &fakeLeaser{}
	rec := &leaseCounter{}
	p := leasedPacer(l, rec, time.Second)
	release, waited, err := p.AcquireSession(t.Context(), "contoso-com.mail.protection.outlook.com", "contoso.com")
	if err != nil {
		t.Fatal(err)
	}
	if l.keys[0] != "@microsoft-eop" {
		t.Errorf("lease taken under %q, want the family key", l.keys[0])
	}
	if l.limits[0] != 1 {
		t.Errorf("limit %d, want the working conc of 1", l.limits[0])
	}
	if waited > 50*time.Millisecond || rec.outcomes[LeaseImmediate] != 1 {
		t.Errorf("waited %s, outcomes %v", waited, rec.outcomes)
	}
	if got := p.Snapshot()[0].Inflight; got != 1 {
		t.Errorf("inflight %d while held, want 1", got)
	}
	release()
	if len(l.released) != 1 || l.released[0] != "@microsoft-eop/lease-1" {
		t.Errorf("released %v", l.released)
	}
	if got := p.Snapshot()[0].Inflight; got != 0 {
		t.Errorf("inflight %d after release, want 0", got)
	}
}

// Two tenants of one family queue for the same room.
func TestTenantsOfOneFamilyShareTheLease(t *testing.T) {
	l := &fakeLeaser{}
	p := leasedPacer(l, nil, time.Second)
	for _, h := range []string{"a-com.mail.protection.outlook.com", "b-de.mail.protection.outlook.com"} {
		release, _, err := p.AcquireSession(t.Context(), h, "x.com")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if l.keys[0] != l.keys[1] || l.keys[0] != "@microsoft-eop" {
		t.Errorf("keys %v, want both @microsoft-eop", l.keys)
	}
}

func TestASessionWaitsForTheRoom(t *testing.T) {
	l := &fakeLeaser{refuse: 2, retry: 10 * time.Millisecond}
	rec := &leaseCounter{}
	p := leasedPacer(l, rec, 5*time.Second)
	release, waited, err := p.AcquireSession(t.Context(), "mx.example.de", "example.de")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if waited <= 0 || len(l.keys) != 3 {
		t.Errorf("waited %s over %d attempts", waited, len(l.keys))
	}
	if rec.outcomes[LeaseAfterWait] != 1 {
		t.Errorf("outcomes %v, want one granted_after_wait", rec.outcomes)
	}
}

// Past the bound the caller is told to come back — a pause, never a verdict.
func TestAWaitPastTheBoundIsAPause(t *testing.T) {
	l := &fakeLeaser{refuse: 1 << 30, retry: 20 * time.Second}
	rec := &leaseCounter{}
	p := leasedPacer(l, rec, 300*time.Millisecond)
	_, _, err := p.AcquireSession(t.Context(), "contoso-com.mail.protection.outlook.com", "contoso.com")
	var pe *PausedError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PausedError", err)
	}
	if pe.MXHost != "@microsoft-eop" || pe.RetryAfter() < 19*time.Second {
		t.Errorf("paused %q for %s, want the family and the earliest expiry", pe.MXHost, pe.RetryAfter())
	}
	if rec.outcomes[LeaseTimedOut] != 1 {
		t.Errorf("outcomes %v", rec.outcomes)
	}
}

// Invariant 5: a lease set that cannot be read means no session.
func TestNoLeaseSetMeansNoSession(t *testing.T) {
	down := errors.New("redis: connection refused")
	p := leasedPacer(&fakeLeaser{err: down}, nil, time.Second)
	release, _, err := p.AcquireSession(t.Context(), "mx.example.de", "example.de")
	if !errors.Is(err, down) || release != nil {
		t.Fatalf("release %v, err %v — want no session and the store's error", release != nil, err)
	}
	if errors.Is(err, ErrPaused) {
		t.Error("an unreadable lease set must be no_budget, not a pause")
	}
}

func TestACancelledWaitStops(t *testing.T) {
	p := leasedPacer(&fakeLeaser{refuse: 1 << 30, retry: time.Second}, nil, time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := p.AcquireSession(ctx, "mx.example.de", "example.de"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestWithoutALeaserSessionsAreUncounted(t *testing.T) {
	p := New(newStore(nil), &fakeTaker{}, Options{})
	release, waited, err := p.AcquireSession(t.Context(), "mx.example.de", "example.de")
	if err != nil || release == nil || waited != 0 {
		t.Fatalf("release %v waited %s err %v", release != nil, waited, err)
	}
	release()
}

// The operator's decision (plan 028, 2026-10-01): one connection per pace key,
// for every key. Every shipped band and the stranger's default say 1, and a
// change to that is a decision, not an edit.
func TestEveryShippedBandAllowsOneConnection(t *testing.T) {
	if c := conservative(); c.MaxConc != 1 || c.MinConc != 1 {
		t.Errorf("conservative() concurrency %d..%d, want 1..1", c.MinConc, c.MaxConc)
	}
	entries, err := fs.ReadDir(seed, "bands")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := seed.ReadFile("bands/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var b Band
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if b.MaxConc != 1 {
			t.Errorf("%s: max_concurrency %d, want 1", e.Name(), b.MaxConc)
		}
	}
}
