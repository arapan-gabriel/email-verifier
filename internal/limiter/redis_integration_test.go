package limiter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// The central bucket against a real Redis (plan 027, Design 5). Invariant 4
// rests on token_bucket.lua, and before this file no test had ever executed it:
// fakeStore re-implemented its arithmetic in Go.
//
// These tests need a Redis and skip without one. CI provides a redis:7-alpine
// service. Locally:
//
//	docker run -d --rm --name ev-test-redis -p 127.0.0.1:56379:6379 redis:7-alpine
//	VERIFIERD_TEST_REDIS_ADDR=127.0.0.1:56379 go test ./internal/limiter/ ./internal/pacer/
//
// (or `make test-redis`). Every test uses a random MX name, so runs never share
// a bucket and a shared Redis is safe to point at.

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

func uniqueHost(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "mx-" + hex.EncodeToString(b[:]) + ".test"
}

// takeAt runs the bucket script with the clock set exactly — the script takes
// the caller's time (ARGV[3]) for precisely this reason.
func takeAt(t *testing.T, l *Limiter, host string, rate, burst, now float64) Decision {
	t.Helper()
	raw, err := l.store.Run(t.Context(), l.script, []string{Key(host)}, []string{
		strconv.FormatFloat(rate, 'f', -1, 64),
		strconv.FormatFloat(burst, 'f', -1, 64),
		strconv.FormatFloat(now, 'f', -1, 64),
		"1",
	})
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	d, err := parseDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

// N takes over T seconds admit exactly burst + rate·T. Steps of 0.25s at 2/s
// are exact in binary floating point, so "exactly" means exactly.
func TestRealBucketAdmitsBurstPlusRateTimesT(t *testing.T) {
	l := New(realRedis(t))
	host := uniqueHost(t)
	const rate, burst, T = 2.0, 3.0, 10.0
	base := 1_000_000_000.0
	admitted := 0
	for k := 0; float64(k)*0.25 <= T; k++ {
		if takeAt(t, l, host, rate, burst, base+float64(k)*0.25).Allowed {
			admitted++
		}
	}
	if want := int(burst + rate*T); admitted != want {
		t.Errorf("admitted %d over %vs, want burst + rate·T = %d", admitted, T, want)
	}
}

// Burst is honoured, and a long idle refills to burst — never beyond it.
func TestRealBucketRefillIsCappedAtBurst(t *testing.T) {
	l := New(realRedis(t))
	host := uniqueHost(t)
	base := 2_000_000_000.0
	count := func(now float64) int {
		n := 0
		for range 20 {
			if takeAt(t, l, host, 1, 4, now).Allowed {
				n++
			}
		}
		return n
	}
	if n := count(base); n != 4 {
		t.Errorf("a fresh bucket admitted %d at one instant, want burst 4", n)
	}
	if n := count(base + 100_000); n != 4 {
		t.Errorf("after a long idle it admitted %d, want burst 4 — refill must cap", n)
	}
	d := takeAt(t, l, host, 1, 4, base+100_000)
	if d.Allowed || d.RetryAfter != time.Second {
		t.Errorf("empty bucket = %+v, want refused with retry 1s at 1/s", d)
	}
}

// 32 goroutines racing one key never over-admit. This is the one-round-trip
// claim: take and refill in one script, so no two callers spend the same token.
func TestRealBucketNeverOverAdmitsUnderRace(t *testing.T) {
	l := New(realRedis(t))
	host := uniqueHost(t)
	now := 3_000_000_000.0 // one instant: no refill, so exactly burst may pass
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				raw, err := l.store.Run(context.Background(), l.script, []string{Key(host)},
					[]string{"0.001", "5", strconv.FormatFloat(now, 'f', -1, 64), "1"})
				if err != nil {
					t.Error(err)
					return
				}
				if d, _ := parseDecision(raw); d.Allowed {
					admitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 5 {
		t.Errorf("256 racing takes admitted %d, want exactly burst 5", n)
	}
}

// The public Take, on the synctest clock: the caller's time.Now is what the
// script sees, so the fake clock drives the real bucket.
func TestRealTakeOnTheFakeClock(t *testing.T) {
	c := realRedis(t)
	host := uniqueHost(t)
	synctest.Test(t, func(t *testing.T) {
		l := New(c)
		admitted := 0
		for range 10 {
			if d, err := l.Take(t.Context(), host, 1, 2); err != nil {
				t.Fatal(err)
			} else if d.Allowed {
				admitted++
			}
			time.Sleep(500 * time.Millisecond)
		}
		// 2 at start, then 1/s for the 4.5s spanned by the ten calls.
		if admitted != 6 {
			t.Errorf("admitted %d, want burst 2 + 4 refilled", admitted)
		}
	})
}

type countingStore struct {
	Store
	calls atomic.Int64
}

func (c *countingStore) Run(ctx context.Context, s *redis.Script, keys, args []string) (any, error) {
	c.calls.Add(1)
	return c.Store.Run(ctx, s, keys, args)
}

// rate or burst ≤ 0 is refused on the Go side, before the script could divide
// by it.
func TestRealTakeRefusesNonPositiveBeforeTheScript(t *testing.T) {
	cs := &countingStore{Store: realRedis(t)}
	l := New(cs)
	for _, rb := range [][2]float64{{0, 1}, {-1, 1}, {1, 0}, {1, -2}} {
		if _, err := l.Take(t.Context(), uniqueHost(t), rb[0], rb[1]); err == nil {
			t.Errorf("Take(rate %v, burst %v) accepted", rb[0], rb[1])
		}
	}
	if n := cs.calls.Load(); n != 0 {
		t.Errorf("the script ran %d times for refused inputs", n)
	}
}

// The fake and the real script agree on the same sequence, so fakeStore cannot
// drift from the Lua it stands in for.
func TestFakeAgreesWithRealScript(t *testing.T) {
	lua := New(realRedis(t))
	type step struct{ rate, burst, dt float64 }
	for name, seq := range map[string][]step{
		"drain then refill": {{1, 2, 0}, {1, 2, 0}, {1, 2, 0}, {1, 2, 0.5}, {1, 2, 0.5}, {1, 2, 0}, {1, 2, 10}, {1, 2, 0}, {1, 2, 0}, {1, 2, 0}},
		"slow rate":         {{0.25, 1, 0}, {0.25, 1, 1}, {0.25, 1, 1}, {0.25, 1, 1}, {0.25, 1, 1}, {0.25, 1, 0}},
		"band changes":      {{4, 4, 0}, {4, 4, 0}, {0.5, 1, 0}, {0.5, 1, 1}, {0.5, 1, 1}, {4, 4, 0.25}, {4, 4, 0}},
		"clock goes back":   {{1, 1, 0}, {1, 1, -5}, {1, 1, 0}, {1, 1, 6}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := New(&fakeStore{})
			host := uniqueHost(t)
			now := 4_000_000_000.0
			for i, s := range seq {
				now += s.dt
				r := takeAt(t, lua, host, s.rate, s.burst, now)
				f := takeAt(t, fake, host, s.rate, s.burst, now)
				if r.Allowed != f.Allowed || r.Tokens != f.Tokens || r.RetryAfter != f.RetryAfter {
					t.Fatalf("step %d: lua %+v, fake %+v", i, r, f)
				}
			}
		})
	}
}

// proxy forwards to Redis until killed, standing in for "the container stops"
// without the test needing Docker control.
type proxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startProxy(t *testing.T, target string) *proxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var d net.Dialer
			up, err := d.DialContext(context.Background(), "tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	t.Cleanup(p.kill)
	return p
}

func (p *proxy) kill() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// Redis going away mid-life makes Take fail — with an error, never a
// permissive default — so the caller fails closed (invariant 5, the 003 gate
// made permanent). Pooled connections included.
func TestRealTakeFailsClosedWhenRedisGoesAway(t *testing.T) {
	realRedis(t) // skip unless configured
	p := startProxy(t, os.Getenv(redisEnv))
	c := redis.New(redis.Options{Addr: p.ln.Addr().String(), Timeout: 500 * time.Millisecond})
	defer func() { _ = c.Close() }()
	l := New(c)
	host := uniqueHost(t)
	if d, err := l.Take(t.Context(), host, 1, 1); err != nil || !d.Allowed {
		t.Fatalf("Take through the live proxy = %+v, %v", d, err)
	}
	p.kill()
	for i := range 3 {
		d, err := l.Take(t.Context(), host, 100, 100)
		if err == nil {
			t.Fatalf("take %d after Redis went away = %+v, want an error", i, d)
		}
		if d.Allowed {
			t.Fatalf("take %d allowed with no Redis", i)
		}
	}
	// A transport failure, not a *redis.Error: the pool must not hand the
	// broken connection back as if Redis had merely rejected a command.
	var rerr *redis.Error
	if _, err := l.Take(t.Context(), host, 1, 1); errors.As(err, &rerr) {
		t.Errorf("a dead Redis surfaced as a protocol error: %v", err)
	}
}
