package resolver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// counting wraps a lookup so tests assert on *lookups performed*, which is what
// the cache is for, rather than on timing, which proves nothing.
func counting(addrs []string, err error) (LookupFunc, *atomic.Int64) {
	var n atomic.Int64
	return func(context.Context, string, string) ([]netip.Addr, error) {
		n.Add(1)
		if err != nil {
			return nil, err
		}
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}, &n
}

func TestCacheServesRepeatLookups(t *testing.T) {
	lookup, n := counting([]string{"142.250.150.27"}, nil)
	r := New(Options{Lookup: lookup})

	for range 10 {
		got, err := r.Resolve(t.Context(), "aspmx.l.google.com")
		if err != nil || len(got) != 1 {
			t.Fatalf("Resolve = %v, %v", got, err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("performed %d lookups for 10 resolutions, want 1", n.Load())
	}
}

// A domain pointing its MX inward should cost one lookup, not one per request.
func TestCacheRemembersRefusals(t *testing.T) {
	lookup, n := counting([]string{"127.0.0.1", "10.0.0.1"}, nil)
	r := New(Options{Lookup: lookup})

	for range 5 {
		_, err := r.Resolve(t.Context(), "evil.example.test")
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("error = %v, want *BlockedError on every call", err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("performed %d lookups for 5 refusals, want 1", n.Load())
	}
}

func TestCacheRemembersFailures(t *testing.T) {
	lookup, n := counting(nil, errors.New("no such host"))
	r := New(Options{Lookup: lookup})
	for range 4 {
		if _, err := r.Resolve(t.Context(), "nx.example.test"); err == nil {
			t.Fatal("expected an error")
		}
	}
	if n.Load() != 1 {
		t.Errorf("performed %d lookups, want 1", n.Load())
	}
}

// Entries expire. synctest advances the clock instead of the suite sleeping.
func TestCacheEntriesExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lookup, n := counting([]string{"142.250.150.27"}, nil)
		r := New(Options{Lookup: lookup, CacheTTL: 5 * time.Minute, NegativeTTL: time.Minute})
		ctx := context.Background()

		if _, err := r.Resolve(ctx, "mx.example.test"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Minute)
		if _, err := r.Resolve(ctx, "mx.example.test"); err != nil {
			t.Fatal(err)
		}
		if n.Load() != 1 {
			t.Fatalf("re-looked-up before the TTL: %d", n.Load())
		}

		time.Sleep(2 * time.Minute) // past 5m total
		if _, err := r.Resolve(ctx, "mx.example.test"); err != nil {
			t.Fatal(err)
		}
		if n.Load() != 2 {
			t.Errorf("performed %d lookups, want 2 — the entry should have expired", n.Load())
		}
	})
}

// A refusal is held briefly, so a misconfiguration that gets fixed is not
// remembered as long as a good answer.
func TestNegativeTTLIsShorterThanPositive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lookup, n := counting([]string{"127.0.0.1"}, nil)
		r := New(Options{Lookup: lookup, CacheTTL: 10 * time.Minute, NegativeTTL: time.Minute})
		ctx := context.Background()

		_, _ = r.Resolve(ctx, "evil.example.test")
		time.Sleep(90 * time.Second)
		_, _ = r.Resolve(ctx, "evil.example.test")
		if n.Load() != 2 {
			t.Errorf("performed %d lookups, want 2 — a refusal must not be held for the positive TTL", n.Load())
		}
	})
}

func TestCacheIsBounded(t *testing.T) {
	lookup, _ := counting([]string{"142.250.150.27"}, nil)
	r := New(Options{Lookup: lookup, CacheSize: 16})
	for i := range 200 {
		if _, err := r.Resolve(t.Context(), fmt.Sprintf("mx%d.example.test", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.cache.len(); got > 16 {
		t.Errorf("cache holds %d entries, cap is 16", got)
	}
}

// Nothing to cache, and caching would be a way to smuggle a refused literal in.
func TestLiteralsAreNotCached(t *testing.T) {
	lookup, n := counting(nil, errors.New("must not be called"))
	r := New(Options{Lookup: lookup})
	for range 3 {
		if _, err := r.Resolve(t.Context(), "142.250.150.27"); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 0 {
		t.Error("a literal triggered a lookup")
	}
	if r.cache.len() != 0 {
		t.Errorf("cache holds %d entries for literals, want 0", r.cache.len())
	}
}

// A slow resolver must not spend the probe's whole budget.
func TestResolutionIsBounded(t *testing.T) {
	r := New(Options{
		Timeout: 50 * time.Millisecond,
		Lookup: func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	start := time.Now()
	if _, err := r.Resolve(context.Background(), "slow.example.test"); err == nil {
		t.Fatal("a hanging resolver returned no error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %s; the resolver timeout was not applied", elapsed)
	}
}

func TestServersAreUsedWhenConfigured(t *testing.T) {
	// A dead server must fail rather than silently fall back to the host's.
	r := New(Options{Servers: []string{"127.0.0.1:1"}, Timeout: 300 * time.Millisecond})
	if _, err := r.Resolve(context.Background(), "example.test"); err == nil {
		t.Error("resolution succeeded against a dead configured server")
	}
}
