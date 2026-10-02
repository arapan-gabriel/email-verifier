package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Plan 028's gate at the lease set: 32 goroutines contending for one pace key
// with a limit of 1 never hold two leases at once — counted in Redis itself, by
// sampling ZCARD while they run — and every one of them eventually gets its turn.
func TestRealLeaseNeverExceedsTheLimit(t *testing.T) {
	c := realRedis(t)
	l := NewLeases(c)
	key := uniqueHost(t)
	ctx := t.Context()

	var stop atomic.Bool
	var maxHeld atomic.Int64
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for !stop.Load() {
			n, err := c.Do(context.Background(), "ZCARD", InflightKey(key))
			if err == nil {
				if v, ok := n.(int64); ok && v > maxHeld.Load() {
					maxHeld.Store(v)
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	var done atomic.Int64
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				d, err := l.Lease(ctx, key, 1, 5*time.Second)
				if err != nil {
					t.Errorf("Lease: %v", err)
					return
				}
				if !d.Granted {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				if d.Held != 1 {
					t.Errorf("granted with %d held, want 1", d.Held)
				}
				time.Sleep(3 * time.Millisecond) // the "session"
				if err := l.Release(ctx, key, d.ID); err != nil {
					t.Errorf("Release: %v", err)
				}
				done.Add(1)
				return
			}
			t.Error("a goroutine never got its turn")
		}()
	}
	wg.Wait()
	stop.Store(true)
	sampler.Wait()

	if got := maxHeld.Load(); got > 1 {
		t.Fatalf("ZCARD reached %d with a limit of 1", got)
	}
	if done.Load() != 32 {
		t.Fatalf("%d of 32 sessions ran", done.Load())
	}
}

// A node that dies holding a lease cannot wedge the family: the lease expires,
// and a refused caller is told when.
func TestRealCrashedHoldersLeaseExpires(t *testing.T) {
	c := realRedis(t)
	l := NewLeases(c)
	key := uniqueHost(t)
	ctx := t.Context()

	held, err := l.Lease(ctx, key, 1, 300*time.Millisecond)
	if err != nil || !held.Granted {
		t.Fatalf("first lease = %+v, %v", held, err)
	}
	// Never released: the holder "crashed".
	refused, err := l.Lease(ctx, key, 1, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if refused.Granted {
		t.Fatal("a second lease was granted while the first was live")
	}
	if refused.RetryAfter <= 0 || refused.RetryAfter > 300*time.Millisecond {
		t.Fatalf("RetryAfter = %s, want (0, 300ms] — the earliest expiry", refused.RetryAfter)
	}
	time.Sleep(350 * time.Millisecond)
	after, err := l.Lease(ctx, key, 1, 300*time.Millisecond)
	if err != nil || !after.Granted {
		t.Fatalf("after the expiry: %+v, %v — the dead holder still owns the family", after, err)
	}
}

// Two pace keys do not share leases: a session at Microsoft does not block one at
// Google.
func TestRealLeasesOfTwoKeysAreIndependent(t *testing.T) {
	c := realRedis(t)
	l := NewLeases(c)
	a, b := uniqueHost(t), uniqueHost(t)
	ctx := t.Context()
	if d, err := l.Lease(ctx, a, 1, time.Second); err != nil || !d.Granted {
		t.Fatalf("key a: %+v, %v", d, err)
	}
	if d, err := l.Lease(ctx, b, 1, time.Second); err != nil || !d.Granted {
		t.Fatalf("key b refused while only a was held: %+v, %v", d, err)
	}
}

// A released lease frees its room at once, without waiting for the expiry.
func TestRealReleaseFreesTheRoom(t *testing.T) {
	c := realRedis(t)
	l := NewLeases(c)
	key := uniqueHost(t)
	ctx := t.Context()
	d, err := l.Lease(ctx, key, 1, time.Minute)
	if err != nil || !d.Granted {
		t.Fatal(d, err)
	}
	if err := l.Release(ctx, key, d.ID); err != nil {
		t.Fatal(err)
	}
	if d2, err := l.Lease(ctx, key, 1, time.Minute); err != nil || !d2.Granted {
		t.Fatalf("after release: %+v, %v", d2, err)
	}
}
