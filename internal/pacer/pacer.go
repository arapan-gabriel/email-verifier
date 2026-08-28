package pacer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/metrics"
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

// PausedError carries how much of the cooldown is left, so the caller can tell
// its own scheduler exactly when to come back instead of guessing.
type PausedError struct {
	MXHost string
	Until  time.Time
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("%s: %s, %s remaining", ErrPaused, e.MXHost, e.RetryAfter().Round(time.Second))
}

// Unwrap makes errors.Is(err, ErrPaused) work.
func (e *PausedError) Unwrap() error { return ErrPaused }

// RetryAfter is the remaining cooldown, never negative.
func (e *PausedError) RetryAfter() time.Duration {
	return max(0, time.Until(e.Until))
}

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

// PauseRecorder is told when an MX is stood down. Nil means nobody is counting.
type PauseRecorder interface {
	Pause(mxHost string)
}

// Promotion controls how evidence that a band's ceiling is too low becomes a
// proposal to raise it.
//
// AIMD moves only inside [min, max]: it halves on a throttle and climbs on
// clean answers, but never past the ceiling. Every shipped band says
// "confidence": "guess", so an MX that tolerates more than its seed says would
// otherwise sit under-used forever with nothing noticing.
type Promotion struct {
	// After is how many consecutive clean answers **at the ceiling** count as
	// evidence. Zero disables proposals.
	After int
	// Step multiplies the current ceiling to form the proposal.
	Step float64
	// Ceiling caps any proposal in absolute terms, so no run of clean answers
	// can propose a rate nobody sanctioned.
	Ceiling float64
}

// Options bounds what the pacer keeps in memory.
type Options struct {
	// IdleTTL drops an MX not asked about for this long. Eviction is lossless:
	// the working point lives in Redis, so an evicted entry costs one re-read.
	IdleTTL time.Duration
	// MaxTracked caps the number of MXes held at once.
	MaxTracked int
	// Metrics counts pause events. Optional.
	Metrics PauseRecorder
	// Promote turns clean answers at the ceiling into a band proposal.
	Promote Promotion
}

// Pacer holds each MX to a rate. Safe for concurrent use.
type Pacer struct {
	store   Store
	take    Taker
	opts    Options
	metrics PauseRecorder

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
	lastUsed    time.Time
	// cleanAtCeiling is the evidence for a proposal: consecutive clean answers
	// while already at the top of the band.
	cleanAtCeiling int
	proposed       bool
}

// New returns a Pacer over the shared bucket and the operational store.
//
// The in-memory map is bounded. It is keyed by a value that arrives in the
// request, so without a bound a bulk run over ten thousand domains would hold
// ten thousand entries for the life of the process — and every per-MX metric
// labelled from it would be a time series that never goes away.
func New(store Store, take Taker, opts Options) *Pacer {
	if opts.IdleTTL <= 0 {
		opts.IdleTTL = 30 * time.Minute
	}
	if opts.MaxTracked <= 0 {
		opts.MaxTracked = 512
	}
	if opts.Promote.Step <= 1 {
		opts.Promote.Step = 1.5
	}
	if opts.Promote.Ceiling <= 0 {
		opts.Promote.Ceiling = 20
	}
	return &Pacer{store: store, take: take, opts: opts, metrics: opts.Metrics, mx: make(map[string]*mxState)}
}

// Snapshot reports what the pacer is currently tracking, for the scrape.
// Gauges are pulled rather than pushed: this is the state, and mirroring it
// would give two answers that can disagree.
func (p *Pacer) Snapshot() []metrics.MXState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]metrics.MXState, 0, len(p.mx))
	for host, st := range p.mx {
		out = append(out, metrics.MXState{
			Host: host, Rate: st.rate, MaxRate: st.band.MaxRate, Conc: st.conc, State: st.state,
		})
	}
	return out
}

// Tracked reports how many MXes are held, for tests and the cardinality gauge.
func (p *Pacer) Tracked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.mx)
}

// evictLocked drops idle entries, then the least recently used if the cap is
// still exceeded. A paused MX is safe to evict: pause_until is in Redis and is
// re-read when the entry comes back.
func (p *Pacer) evictLocked() {
	cutoff := time.Now().Add(-p.opts.IdleTTL)
	for host, st := range p.mx {
		if st.lastUsed.Before(cutoff) {
			delete(p.mx, host)
		}
	}
	for len(p.mx) >= p.opts.MaxTracked {
		var oldest string
		var oldestAt time.Time
		for host, st := range p.mx {
			if oldest == "" || st.lastUsed.Before(oldestAt) {
				oldest, oldestAt = host, st.lastUsed
			}
		}
		if oldest == "" {
			return
		}
		delete(p.mx, oldest)
	}
}

// Acquire blocks a probe until this MX has budget for it.
//
// **It fails closed** (invariant 5): if the bucket cannot be consulted, the
// error is returned and the caller must skip the probe. An unconfirmed verdict
// is recoverable; a blocklist entry is not.
func (p *Pacer) Acquire(ctx context.Context, mxHost, domain string) error {
	st := p.stateFor(ctx, mxHost, domain)

	p.mu.Lock()
	if time.Now().Before(st.pausedUntil) {
		until := st.pausedUntil
		p.mu.Unlock()
		return &PausedError{MXHost: mxHost, Until: until}
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
	st.lastUsed = time.Now()
	paused, propose := false, false

	switch {
	case throttled:
		st.clean = 0
		// One real throttle and the ceiling is not proven after all.
		st.cleanAtCeiling = 0
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
		paused = true

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
		// Evidence only counts while already at the top of the band: answering
		// cleanly below the ceiling says nothing about whether the ceiling is
		// the limit.
		if st.rate >= st.band.MaxRate {
			st.cleanAtCeiling++
			if p.opts.Promote.After > 0 && !st.proposed && st.cleanAtCeiling >= p.opts.Promote.After {
				propose = true
				st.proposed = true
			}
		}
	}

	snapshot := *st
	p.mu.Unlock()
	if paused && p.metrics != nil {
		p.metrics.Pause(mxHost)
	}
	if propose {
		p.proposeBand(ctx, mxHost, snapshot)
	}
	p.persist(ctx, mxHost, snapshot)
}

// Proposal is evidence that a band's ceiling is lower than the provider's, and
// nothing more. It is never applied automatically: AIMD can undo a rate that
// turned out too high *within* a band, but it cannot undo a band widened
// wrongly, and the failure mode of a band that is too wide is a blocklisting
// rather than a slow run.
type Proposal struct {
	MXHost      string  `json:"mx_host"`
	CurrentMax  float64 `json:"current_max_rate_per_sec"`
	ProposedMax float64 `json:"proposed_max_rate_per_sec"`
	CleanAt     int     `json:"clean_answers_at_ceiling"`
	FormedAt    int64   `json:"formed_at"`
}

// ProposalKey is where a proposal for one MX lives.
func ProposalKey(mxHost string) string { return "limits:mx:" + mxHost + ":proposed" }

func (p *Pacer) proposeBand(ctx context.Context, mxHost string, st mxState) {
	next := min(p.opts.Promote.Ceiling, st.band.MaxRate*p.opts.Promote.Step)
	if next <= st.band.MaxRate {
		return // already at the absolute ceiling; there is nothing to propose
	}
	body, err := json.Marshal(Proposal{
		MXHost: mxHost, CurrentMax: st.band.MaxRate, ProposedMax: next,
		CleanAt: st.cleanAtCeiling, FormedAt: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	_ = p.store.Set(ctx, ProposalKey(mxHost), string(body))
}

// Proposal returns the standing proposal for an MX, if any.
func (p *Pacer) Proposal(ctx context.Context, mxHost string) (Proposal, bool) {
	raw, ok, err := p.store.Get(ctx, ProposalKey(mxHost))
	if err != nil || !ok || raw == "" { // "" is how Promote clears one
		return Proposal{}, false
	}
	var pr Proposal
	if json.Unmarshal([]byte(raw), &pr) != nil {
		return Proposal{}, false
	}
	return pr, true
}

// Promote applies a standing proposal: it widens the band on disk, clears the
// proposal, and drops the in-memory entry so the next request reads the new
// band. This is the operator's decision, never the loop's.
func (p *Pacer) Promote(ctx context.Context, mxHost string) (Proposal, error) {
	pr, ok := p.Proposal(ctx, mxHost)
	if !ok {
		return Proposal{}, fmt.Errorf("pacer: no standing proposal for %s", mxHost)
	}

	band := p.bandFor(ctx, mxHost, "")
	band.MaxRate = pr.ProposedMax
	body, err := json.Marshal(band)
	if err != nil {
		return Proposal{}, err
	}
	if err := p.store.Set(ctx, "limits:mx:"+mxHost, string(body)); err != nil {
		return Proposal{}, fmt.Errorf("pacer: writing the promoted band: %w", err)
	}
	if err := p.store.Set(ctx, ProposalKey(mxHost), ""); err != nil {
		return Proposal{}, err
	}

	p.mu.Lock()
	delete(p.mx, mxHost) // the next request re-reads; no restart needed
	p.mu.Unlock()
	return pr, nil
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
		st.lastUsed = time.Now()
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
		st.lastUsed = time.Now()
		return st
	}
	p.evictLocked()
	st := &mxState{band: band, rate: start, conc: band.MinConc, state: StateProbing, lastUsed: time.Now()}
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
