package pacer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// The pacer over the real central bucket and the real operational store (plan
// 027, Design 5). Skips unless VERIFIERD_TEST_REDIS_ADDR is set; CI sets it.
// See internal/limiter/redis_integration_test.go for the one-line docker run.

const redisEnv = "VERIFIERD_TEST_REDIS_ADDR"

func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv(redisEnv)
	if addr == "" {
		t.Skipf("%s is not set; start one with `docker run -d --rm -p 127.0.0.1:56379:6379 redis:7-alpine` "+
			"and set %s=127.0.0.1:56379 (CI always sets it)", redisEnv, redisEnv)
	}
	c := redis.New(redis.Options{Addr: addr, Timeout: 2 * time.Second, PoolSize: 64})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("%s=%s is set but Redis does not answer: %v", redisEnv, addr, err)
	}
	return c
}

// uniqueMX names an MX no other run has touched, and gives it a known band
// through the public store path (limits:mx:<host>, what plan 012 writes).
func uniqueMX(t *testing.T, c *redis.Client, band string) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	host := "mx-" + hex.EncodeToString(b[:]) + ".test"
	if err := c.Set(t.Context(), "limits:mx:"+host, band); err != nil {
		t.Fatal(err)
	}
	return host
}

// 32 goroutines acquiring one MX through the real bucket never get more than
// the band allows: burst at once, then nothing until the (tiny) rate refills.
func TestRealPacerNeverOverAdmits(t *testing.T) {
	c := realRedis(t)
	host := uniqueMX(t, c, `{"min_rate_per_sec":0.001,"max_rate_per_sec":0.001,"burst":5,"pause_seconds":60}`)
	p := New(c, limiter.New(c), Options{})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := p.Acquire(ctx, host, "example.test"); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Acquire = %v", err)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 5 {
		t.Errorf("32 racing acquires admitted %d, want exactly burst 5", n)
	}
}

// A backed-off rate and a pause both reach Redis, and a new process on the same
// Redis — a restart, or a second node — resumes from them: no higher rate, and
// no token taken while the pause stands.
func TestRealPacerStateSurvivesARestart(t *testing.T) {
	c := realRedis(t)
	host := uniqueMX(t, c, testBand) // 0.5..4/s, pause 300s
	ctx := t.Context()
	first := New(c, limiter.New(c), Options{})
	if err := first.Acquire(ctx, host, "example.test"); err != nil {
		t.Fatal(err)
	}
	first.Observe(ctx, host, true) // 4 → 2
	second := New(c, limiter.New(c), Options{})
	if err := second.Acquire(ctx, host, "example.test"); err != nil {
		t.Fatal(err)
	}
	if r, _ := second.Rate(host); r != 2 {
		t.Errorf("restarted rate = %v, want the persisted 2", r)
	}

	for range 3 { // 2 → 1 → 0.5 (floor) → pause
		first.Observe(ctx, host, true)
	}
	tk := &countingTaker{inner: limiter.New(c)}
	third := New(c, tk, Options{})
	var paused *PausedError
	if err := third.Acquire(ctx, host, "example.test"); !errors.As(err, &paused) {
		t.Fatalf("Acquire during a persisted pause = %v, want PausedError", err)
	}
	if tk.n.Load() != 0 {
		t.Errorf("a restarted pacer took %d tokens during a persisted pause", tk.n.Load())
	}
}

type countingTaker struct {
	inner Taker
	n     atomic.Int64
}

func (c *countingTaker) Take(ctx context.Context, host string, rate, burst float64) (limiter.Decision, error) {
	c.n.Add(1)
	return c.inner.Take(ctx, host, rate, burst)
}

// With Redis unreachable, Acquire fails — an error, never a permissive
// default (invariant 5). Runs without VERIFIERD_TEST_REDIS_ADDR: an address
// nothing listens on is exactly "the container is stopped".
func TestPacerFailsClosedWithRedisDown(t *testing.T) {
	c := redis.New(redis.Options{Addr: "127.0.0.1:1", Timeout: 200 * time.Millisecond})
	defer func() { _ = c.Close() }()
	p := New(c, limiter.New(c), Options{})
	for range 3 {
		if err := p.Acquire(t.Context(), "mx.test", "example.test"); err == nil {
			t.Fatal("Acquire succeeded with Redis down")
		}
	}
}

// Plan 026's gate at the bucket: 32 goroutines acquiring through two
// *different* EOP tenant hostnames get, in aggregate, exactly what one host
// would — the burst, then nothing until the tiny rate refills. Before 026 each
// tenant had a bucket of its own and this admitted twice the burst.
func TestRealFamilyNeverOverAdmitsAcrossTenants(t *testing.T) {
	c := realRedis(t)
	ctx := t.Context()
	const key = "@microsoft-eop"
	clean := func() {
		for _, k := range []string{"limits:mx:" + key, limiter.Key(key), "rt:mx:" + key + ":rate",
			"rt:mx:" + key + ":conc", "rt:mx:" + key + ":state", "rt:mx:" + key + ":pause_until"} {
			if _, err := c.Do(context.Background(), "DEL", k); err != nil {
				t.Fatal(err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	if err := c.Set(ctx, "limits:mx:"+key,
		`{"min_rate_per_sec":0.001,"max_rate_per_sec":0.001,"burst":5,"pause_seconds":60}`); err != nil {
		t.Fatal(err)
	}

	p := New(c, limiter.New(c), Options{})
	tenants := [2]string{"contoso-com.mail.protection.outlook.com", "fabrikam-de.mail.protection.outlook.com"}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := p.Acquire(ctx, tenants[i%2], "example.test"); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Acquire = %v", err)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 5 {
		t.Errorf("32 acquires over two tenants admitted %d, want exactly the family's burst 5", n)
	}
	for _, tenant := range tenants {
		if n, err := c.Do(ctx, "EXISTS", limiter.Key(tenant)); err != nil || n != int64(0) {
			t.Errorf("a per-tenant bucket %s was created", limiter.Key(tenant))
		}
	}
}
