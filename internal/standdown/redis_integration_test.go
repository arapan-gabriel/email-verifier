package standdown

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// Two nodes over one Redis add their refusing hosts together (invariant 4): two
// tenants seen by node A and one by node B are three distinct hosts.
func TestTwoNodesAddUp(t *testing.T) {
	addr := os.Getenv("VERIFIERD_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("VERIFIERD_TEST_REDIS_ADDR is not set")
	}
	c := redis.New(redis.Options{Addr: addr, Timeout: 2 * time.Second})
	t.Cleanup(func() { _ = c.Close() })
	clean := func() { _, _ = c.Do(context.Background(), "DEL", RefusalsKey("@microsoft-eop")) }
	clean()
	t.Cleanup(clean)

	now := time.Now()
	pa, pb := &fakePauser{}, &fakePauser{}
	opts := Options{Hosts: 3, Window: 30 * time.Minute, Pause: time.Hour, Now: func() time.Time { return now }}
	a := New(c, pa, nil, opts)
	b := New(c, pb, nil, opts)

	a.Observe(t.Context(), tenant(1), unexplained)
	a.Observe(t.Context(), tenant(2), unexplained)
	if rule := b.Observe(t.Context(), tenant(3), unexplained); rule != "B" {
		t.Fatalf("node B saw rule %q, want B from three hosts across two nodes", rule)
	}
	if len(pa.paused) != 0 || len(pb.paused) != 1 {
		t.Errorf("pauses a=%v b=%v", pa.paused, pb.paused)
	}
}
