// Package limiter is the shared per-MX token bucket.
//
// The bucket is central by design (invariant 4, ADR-004): every probe node
// draws from one bucket per recipient MX, because N nodes with local buckets
// means N times the intended rate at the provider. Take and refill happen in a
// single Lua call — with two round trips, concurrent workers read the same
// token count and both spend it, which is how "one request per three seconds"
// quietly becomes N per three seconds.
package limiter

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// tokenBucket is embedded rather than read from disk: the release artifact is
// one static binary (ADR-005), and a limiter that fails because a file is
// missing would fail open in the worst possible place.
//
//go:embed token_bucket.lua
var tokenBucket string

// Key returns the bucket key for an MX host.
func Key(mxHost string) string { return "rt:mx:" + mxHost + ":bucket" }

// Decision is the outcome of one take.
type Decision struct {
	Allowed    bool
	Tokens     float64
	RetryAfter time.Duration
}

// Store is the subset of the Redis client this package uses. Declared here, in
// the consumer (ENGINEERING-STANDARDS §2).
type Store interface {
	Run(ctx context.Context, s *redis.Script, keys, args []string) (any, error)
}

// Limiter takes tokens from the shared bucket.
type Limiter struct {
	store  Store
	script *redis.Script
}

// New returns a Limiter over the given store.
func New(store Store) *Limiter {
	return &Limiter{store: store, script: redis.NewScript(tokenBucket)}
}

// Take asks for one token's worth of budget against mxHost.
//
// An error here means the budget could not be established, and the caller must
// **fail closed** — skip the probe and report it as unattempted (invariant 5).
// An unconfirmed verdict is recoverable; a blocklist entry is not.
func (l *Limiter) Take(ctx context.Context, mxHost string, rate, burst float64) (Decision, error) {
	if mxHost == "" {
		return Decision{}, errors.New("limiter: mx host is empty")
	}
	if rate <= 0 || burst <= 0 {
		return Decision{}, fmt.Errorf("limiter: rate and burst must be positive, got %v/%v", rate, burst)
	}

	// The clock is the caller's, not Redis TIME, so tests control it and every
	// node agrees on the same seconds.
	now := float64(time.Now().UnixNano()) / float64(time.Second)

	raw, err := l.store.Run(ctx, l.script,
		[]string{Key(mxHost)},
		[]string{
			strconv.FormatFloat(rate, 'f', -1, 64),
			strconv.FormatFloat(burst, 'f', -1, 64),
			strconv.FormatFloat(now, 'f', -1, 64),
			"1",
		})
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: take for %s: %w", mxHost, err)
	}
	return parseDecision(raw)
}

// parseDecision reads {allowed, tokens_left, retry_after_seconds}.
func parseDecision(raw any) (Decision, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) != 3 {
		return Decision{}, fmt.Errorf("limiter: unexpected reply %#v", raw)
	}
	allowed, ok := arr[0].(int64)
	if !ok {
		return Decision{}, fmt.Errorf("limiter: allowed is %T, want integer", arr[0])
	}
	tokens, err := asFloat(arr[1])
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: tokens: %w", err)
	}
	retry, err := asFloat(arr[2])
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: retry_after: %w", err)
	}
	return Decision{
		Allowed:    allowed == 1,
		Tokens:     tokens,
		RetryAfter: time.Duration(retry * float64(time.Second)),
	}, nil
}

func asFloat(v any) (float64, error) {
	switch t := v.(type) {
	case string:
		return strconv.ParseFloat(t, 64)
	case int64:
		return float64(t), nil
	default:
		return 0, fmt.Errorf("value is %T, want string or integer", v)
	}
}
