package limiter

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// leaseScript and releaseScript are embedded for the reason tokenBucket is: the
// artifact is one binary, and a lease that cannot load its script must fail
// closed rather than open sessions unbounded.
//
//go:embed lease.lua
var leaseScript string

const releaseScript = `return redis.call('ZREM', KEYS[1], ARGV[1])`

// InflightKey is where the session leases of one pace key live (plan 028).
func InflightKey(paceKey string) string { return "rt:mx:" + paceKey + ":inflight" }

// LeaseDecision is the outcome of one attempt to open a session.
type LeaseDecision struct {
	Granted bool
	ID      string
	// Held is how many leases the key holds after this attempt.
	Held int
	// RetryAfter is when the earliest current lease expires — the longest a
	// refused caller needs to wait if nobody releases sooner.
	RetryAfter time.Duration
}

// Leases hands out session leases from the shared set. Like the bucket it is
// central — every node draws from the same set — because a per-process
// semaphore would let N nodes hold N connections to one receiver (invariant 4).
type Leases struct {
	store   Store
	lease   *redis.Script
	release *redis.Script
}

// NewLeases returns a lease set over the given store.
func NewLeases(store Store) *Leases {
	return &Leases{store: store, lease: redis.NewScript(leaseScript), release: redis.NewScript(releaseScript)}
}

// Lease asks for one of `limit` concurrent sessions on paceKey, lasting ttl
// unless released. An error means the count could not be established, and the
// caller must open no session (invariant 5).
func (l *Leases) Lease(ctx context.Context, paceKey string, limit int, ttl time.Duration) (LeaseDecision, error) {
	if paceKey == "" {
		return LeaseDecision{}, errors.New("limiter: pace key is empty")
	}
	if limit < 1 || ttl <= 0 {
		return LeaseDecision{}, fmt.Errorf("limiter: lease limit and ttl must be positive, got %d/%s", limit, ttl)
	}
	id, err := leaseID()
	if err != nil {
		return LeaseDecision{}, err
	}
	now := time.Now().UnixMilli()
	raw, err := l.store.Run(ctx, l.lease,
		[]string{InflightKey(paceKey)},
		[]string{strconv.Itoa(limit), strconv.FormatInt(now, 10), strconv.FormatInt(ttl.Milliseconds(), 10), id})
	if err != nil {
		return LeaseDecision{}, fmt.Errorf("limiter: lease for %s: %w", paceKey, err)
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 3 {
		return LeaseDecision{}, fmt.Errorf("limiter: unexpected lease reply %#v", raw)
	}
	granted, ok1 := arr[0].(int64)
	held, ok2 := arr[1].(int64)
	retry, err := asFloat(arr[2])
	if !ok1 || !ok2 || err != nil {
		return LeaseDecision{}, fmt.Errorf("limiter: unexpected lease reply %#v", raw)
	}
	d := LeaseDecision{Granted: granted == 1, Held: int(held), RetryAfter: time.Duration(retry) * time.Millisecond}
	if d.Granted {
		d.ID = id
	}
	return d, nil
}

// Release gives a lease back. Best-effort by design: a release that fails
// leaves the lease to expire on its own, which costs the family at most one TTL.
func (l *Leases) Release(ctx context.Context, paceKey, id string) error {
	if id == "" {
		return nil
	}
	_, err := l.store.Run(ctx, l.release, []string{InflightKey(paceKey)}, []string{id})
	if err != nil {
		return fmt.Errorf("limiter: release for %s: %w", paceKey, err)
	}
	return nil
}

func leaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("limiter: lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
