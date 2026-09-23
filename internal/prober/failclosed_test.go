package prober

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// The 003 gate made permanent (plan 027): the real pacer and the real limiter,
// wired as main wires them, over a Redis that is not there. Every address comes
// back no_budget and the prober opens ZERO connections (invariant 5). An
// address nothing listens on is exactly what a stopped Redis looks like.
func TestRedisDownMeansZeroDials(t *testing.T) {
	store := redis.New(redis.Options{Addr: "127.0.0.1:1", Timeout: 200 * time.Millisecond})
	defer func() { _ = store.Close() }()
	var dials atomic.Int64
	p := New(Options{
		Pacer:    pacer.New(store, limiter.New(store), pacer.Options{}),
		Resolver: stubResolver{},
		Dialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, net.ErrClosed
		}),
		Timeout: 5 * time.Second,
	})
	resp, err := p.Probe(t.Context(), Request{
		MXHost: "mx.test", Domain: "example.test", NeedCatchAll: true,
		Emails: []string{"a@example.test", "b@example.test", "c@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("opened %d connections with no rate budget (invariant 5)", n)
	}
	for addr, r := range resp.Results {
		if r.Class != ClassNoBudget || r.Accepted != nil {
			t.Errorf("%s: %+v, want no_budget with no verdict", addr, r)
		}
	}
}
