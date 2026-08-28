package pacer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
)

// State names match the Redis contract (rt:mx:<host>:state).
const (
	StateProbing = "PROBING"
	StateSteady  = "STEADY"
	StateBackoff = "BACKOFF"
	StatePaused  = "PAUSED"
)

// cleanBeforeClimb is how many consecutive good answers earn a rate increase.
// Climbing on every clean answer would ramp back into the ceiling that just
// throttled us before the provider's window has moved.
const cleanBeforeClimb = 10

const (
	backoffFactor = 0.5
	climbFactor   = 1.1
)

// ErrPaused means this MX is in its cooldown. It is a refusal to send, never a
// verdict: the caller reports the addresses as unattempted.
var ErrPaused = errors.New("pacer: mx is paused")

// Store is what the pacer needs from Redis. Declared in the consumer
// (ENGINEERING-STANDARDS §2).
type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, val string) error
}

// Taker is the shared bucket.
type Taker interface {
	Take(ctx context.Context, mxHost string, rate, burst float64) (limiter.Decision, error)
}

// Pacer holds each MX to a rate. Safe for concurrent use.
type Pacer struct {
	store Store
	take  Taker

	mu sync.Mutex
	mx map[string]*mxState
}

type mxState struct {
	band        Band
	rate        float64
	conc        int
	clean       int
	state       string
	pausedUntil time.Time
}

// New returns a Pacer over the shared bucket and the operational store.
func New(store Store, take Taker) *Pacer {
	return &Pacer{store: store, take: take, mx: make(map[string]*mxState)}
}

// Acquire blocks a probe until this MX has budget for it.
//
// **It fails closed** (invariant 5): if the bucket cannot be consulted, the
// error is returned and the caller must skip the probe. An unconfirmed verdict
// is recoverable; a blocklist entry is not.
func (p *Pacer) Acquire(ctx context.Context, mxHost, domain string) error {
	st := p.stateFor(ctx, mxHost, domain)

	p.mu.Lock()
	if now := time.Now(); now.Before(st.pausedUntil) {
		left := st.pausedUntil.Sub(now)
		p.mu.Unlock()
		return fmt.Errorf("%w for %s: %s remaining", ErrPaused, mxHost, left.Round(time.Second))
	}
	rate, burst := st.rate, st.band.Burst
	p.mu.Unlock()

	for {
		d, err := p.take.Take(ctx, mxHost, rate, burst)
		if err != nil {
			return fmt.Errorf("pacer: no budget established for %s: %w", mxHost, err)
		}
		if d.Allowed {
			return nil
		}
		wait := d.RetryAfter
		if wait <= 0 {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Observe feeds one answer back into the loop.
//
// The signature is deliberately `throttled bool` and nothing else. Only a
// genuine rate signal may move the pacer (invariant 6): greylisting and
// `4.2.2` over-quota are per-recipient and rate-independent, and a `5.7.x`
// policy block is about our IP — slowing down does not grow a PTR record, and
// if it counted, one blocked IP would calibrate every provider to zero. By
// taking a bool derived from Class.IsThrottle, this package cannot be handed
// the wrong signal by mistake.
func (p *Pacer) Observe(ctx context.Context, mxHost string, throttled bool) {
	p.mu.Lock()
	st, ok := p.mx[mxHost]
	if !ok {
		p.mu.Unlock()
		return
	}

	switch {
	case throttled:
		st.clean = 0
		if st.rate > st.band.MinRate {
			st.rate = max(st.band.MinRate, st.rate*backoffFactor)
			st.conc = max(st.band.MinConc, st.conc-1)
			st.state = StateBackoff
			break
		}
		// Already at the floor and still being throttled: stand down entirely
		// rather than keep poking a server that has said no.
		st.pausedUntil = time.Now().Add(st.band.pauseFor())
		st.state = StatePaused

	default:
		st.clean++
		if st.clean >= cleanBeforeClimb && st.rate < st.band.MaxRate {
			st.clean = 0
			st.rate = min(st.band.MaxRate, st.rate*climbFactor)
			st.conc = min(st.band.MaxConc, st.conc+1)
		}
		if st.state != StatePaused {
			st.state = StateSteady
		}
	}

	snapshot := *st
	p.mu.Unlock()
	p.persist(ctx, mxHost, snapshot)
}

// Rate reports the rate currently settled on, for tests and observability.
func (p *Pacer) Rate(mxHost string) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.mx[mxHost]
	if !ok {
		return 0, false
	}
	return st.rate, true
}

// State reports the pacer state for an MX.
func (p *Pacer) State(mxHost string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.mx[mxHost]; ok {
		return st.state
	}
	return ""
}

func (p *Pacer) stateFor(ctx context.Context, mxHost, domain string) *mxState {
	p.mu.Lock()
	if st, ok := p.mx[mxHost]; ok {
		p.mu.Unlock()
		return st
	}
	p.mu.Unlock()

	band := p.bandFor(ctx, mxHost, domain)
	start := band.MaxRate
	// A rate persisted by an earlier run may only ever *lower* the start.
	// Backing off is a measurement; a quiet hour below the ceiling is not
	// evidence that the ceiling moved.
	if saved, ok := p.savedRate(ctx, mxHost); ok && saved < start {
		start = max(band.MinRate, saved)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.mx[mxHost]; ok { // another goroutine won the race
		return st
	}
	st := &mxState{band: band, rate: start, conc: band.MinConc, state: StateProbing}
	if until, ok := p.savedPause(ctx, mxHost); ok {
		st.pausedUntil = until
		if time.Now().Before(until) {
			st.state = StatePaused
		}
	}
	p.mx[mxHost] = st
	return st
}

func (p *Pacer) savedRate(ctx context.Context, mxHost string) (float64, bool) {
	raw, ok, err := p.store.Get(ctx, "rt:mx:"+mxHost+":rate")
	if err != nil || !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func (p *Pacer) savedPause(ctx context.Context, mxHost string) (time.Time, bool) {
	raw, ok, err := p.store.Get(ctx, "rt:mx:"+mxHost+":pause_until")
	if err != nil || !ok {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// persist publishes the working point so a restart, and a second node, resume
// from it rather than from the ceiling.
func (p *Pacer) persist(ctx context.Context, mxHost string, st mxState) {
	base := "rt:mx:" + mxHost + ":"
	_ = p.store.Set(ctx, base+"rate", strconv.FormatFloat(st.rate, 'f', -1, 64))
	_ = p.store.Set(ctx, base+"conc", strconv.Itoa(st.conc))
	_ = p.store.Set(ctx, base+"state", st.state)
	if !st.pausedUntil.IsZero() {
		_ = p.store.Set(ctx, base+"pause_until", strconv.FormatInt(st.pausedUntil.Unix(), 10))
	}
}
