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

// State names match the Redis contract (rt:mx:<key>:state, key = PaceKey(host)).
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

// Leaser hands out central session leases (plan 028). Declared here, in the
// consumer; *limiter.Leases implements it.
type Leaser interface {
	Lease(ctx context.Context, paceKey string, limit int, ttl time.Duration) (limiter.LeaseDecision, error)
	Release(ctx context.Context, paceKey, id string) error
}

// LeaseRecorder counts lease waits by outcome. Optional.
type LeaseRecorder interface {
	LeaseWait(outcome string)
}

// Lease-wait outcomes, bounded for the metric.
const (
	LeaseImmediate    = "immediate"
	LeaseAfterWait    = "granted_after_wait"
	LeaseTimedOut     = "timed_out"
	defaultLeaseWait  = 60 * time.Second
	defaultLeaseTTL   = 50 * time.Second
	leaseReleaseGrace = 5 * time.Second
)

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

	// Leases enforces the band's concurrency (plan 028): at most `conc`
	// sessions open at once per pace key, counted centrally. Nil means
	// AcquireSession grants without counting — acceptable only in a unit test.
	Leases Leaser
	// LeaseTTL is how long a lease lives if its holder never releases it (a
	// crash). It must outlive the longest session, or a live holder's lease is
	// handed out twice; config validates that.
	LeaseTTL time.Duration
	// LeaseWait bounds how long a session waits for a lease before its addresses
	// come back unattempted with a retry hint.
	LeaseWait time.Duration
	// LeaseMetrics counts waits. Optional.
	LeaseMetrics LeaseRecorder
}

// Pacer holds each MX to a rate. Safe for concurrent use.
type Pacer struct {
	store   Store
	take    Taker
	opts    Options
	metrics PauseRecorder

	mu sync.Mutex
	mx map[string]*mxState
	// inflight counts the leases this process holds, per pace key, for the
	// verify_inflight gauge. The authoritative count is the central set.
	inflight map[string]int
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
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultLeaseTTL
	}
	if opts.LeaseWait <= 0 {
		opts.LeaseWait = defaultLeaseWait
	}
	return &Pacer{
		store: store, take: take, opts: opts, metrics: opts.Metrics,
		mx: make(map[string]*mxState), inflight: make(map[string]int),
	}
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
			Inflight: p.inflight[host],
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

// AcquireSession blocks until this MX's receiving system has room for one more
// open SMTP session, and returns the function that gives the room back (plan
// 028). The limit is the AIMD working `conc` — bounded by the band's
// `[min_concurrency, max_concurrency]`, which every shipped band sets to 1 — so
// a throttle that lowers `conc` lowers the sessions too.
//
// The room is a lease in a central set under the host's pace key, so every node
// and every tenant hostname of one system share it. It fails closed (invariant
// 5): a lease set that cannot be read means no session. Past LeaseWait the
// caller gets a *PausedError naming when the earliest lease expires, which the
// prober reports as unattempted with a retry hint — never a verdict.
func (p *Pacer) AcquireSession(ctx context.Context, mxHost, domain string) (release func(), waited time.Duration, err error) {
	key := PaceKey(mxHost)
	if p.opts.Leases == nil {
		return func() {}, 0, nil
	}
	st := p.stateFor(ctx, key, domain)
	p.mu.Lock()
	limit := max(1, st.conc)
	if until := st.pausedUntil; time.Now().Before(until) {
		// A stood-down key (plan 032) or a floored one: no lease is worth
		// waiting for, and holding one would only delay a sibling's refusal.
		p.mu.Unlock()
		return nil, 0, &PausedError{MXHost: key, Until: until}
	}
	p.mu.Unlock()

	start := time.Now()
	deadline := start.Add(p.opts.LeaseWait)
	refused := false
	for {
		d, err := p.opts.Leases.Lease(ctx, key, limit, p.opts.LeaseTTL)
		if err != nil {
			return nil, time.Since(start), fmt.Errorf("pacer: no session lease for %s: %w", key, err)
		}
		if d.Granted {
			waited = time.Since(start)
			p.recordLease(refused)
			p.mu.Lock()
			p.inflight[key]++
			p.mu.Unlock()
			id := d.ID
			return func() {
				p.mu.Lock()
				if p.inflight[key] > 0 {
					p.inflight[key]--
				}
				p.mu.Unlock()
				// A fresh context: the session's may already be cancelled, and the
				// lease must still go back. A failed release expires on its own.
				rctx, cancel := context.WithTimeout(context.Background(), leaseReleaseGrace)
				defer cancel()
				_ = p.opts.Leases.Release(rctx, key, id)
			}, waited, nil
		}
		refused = true
		wait := d.RetryAfter
		if wait <= 0 || wait > 250*time.Millisecond {
			// Poll rather than sleep to the earliest expiry: a holder normally
			// releases long before its lease would expire.
			wait = 250 * time.Millisecond
		}
		if time.Now().Add(wait).After(deadline) {
			if p.opts.LeaseMetrics != nil {
				p.opts.LeaseMetrics.LeaseWait(LeaseTimedOut)
			}
			retry := d.RetryAfter
			if retry <= 0 {
				retry = time.Second
			}
			return nil, time.Since(start), &PausedError{MXHost: key, Until: time.Now().Add(retry)}
		}
		select {
		case <-ctx.Done():
			return nil, time.Since(start), ctx.Err()
		case <-time.After(wait):
		}
	}
}

// recordLease counts how a granted lease was obtained: at the first ask, or
// after at least one refusal.
func (p *Pacer) recordLease(afterRefusal bool) {
	if p.opts.LeaseMetrics == nil {
		return
	}
	if afterRefusal {
		p.opts.LeaseMetrics.LeaseWait(LeaseAfterWait)
		return
	}
	p.opts.LeaseMetrics.LeaseWait(LeaseImmediate)
}

// Acquire blocks a probe until this MX has budget for it.
//
// Budget is drawn under the host's pace key (PaceKey), not the host: every
// tenant of one receiving system draws from that system's bucket. Callers keep
// passing the real host — the mapping happens here, once, so the SSRF guard,
// the dial and the journal still see the hostname (plan 026).
//
// **It fails closed** (invariant 5): if the bucket cannot be consulted, the
// error is returned and the caller must skip the probe. An unconfirmed verdict
// is recoverable; a blocklist entry is not.
func (p *Pacer) Acquire(ctx context.Context, mxHost, domain string) error {
	key := PaceKey(mxHost)
	st := p.stateFor(ctx, key, domain)

	p.mu.Lock()
	if time.Now().Before(st.pausedUntil) {
		until := st.pausedUntil
		p.mu.Unlock()
		return &PausedError{MXHost: key, Until: until}
	}
	rate, burst := st.rate, st.band.Burst
	p.mu.Unlock()

	for {
		d, err := p.take.Take(ctx, key, rate, burst)
		if err != nil {
			return fmt.Errorf("pacer: no budget established for %s: %w", key, err)
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
	key := PaceKey(mxHost)
	p.mu.Lock()
	st, ok := p.mx[key]
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
		p.metrics.Pause(key)
	}
	if propose {
		p.proposeBand(ctx, key, snapshot)
	}
	p.persist(ctx, key, snapshot)
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

// ProposalKey is where a proposal for one MX lives — under its pace key, so
// every tenant of a family shares one proposal.
func ProposalKey(mxHost string) string { return "limits:mx:" + PaceKey(mxHost) + ":proposed" }

func (p *Pacer) proposeBand(ctx context.Context, key string, st mxState) {
	next := min(p.opts.Promote.Ceiling, st.band.MaxRate*p.opts.Promote.Step)
	if next <= st.band.MaxRate {
		return // already at the absolute ceiling; there is nothing to propose
	}
	body, err := json.Marshal(Proposal{
		MXHost: key, CurrentMax: st.band.MaxRate, ProposedMax: next,
		CleanAt: st.cleanAtCeiling, FormedAt: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	_ = p.store.Set(ctx, ProposalKey(key), string(body))
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
	key := PaceKey(mxHost)
	pr, ok := p.Proposal(ctx, key)
	if !ok {
		return Proposal{}, fmt.Errorf("pacer: no standing proposal for %s", key)
	}

	band := p.bandFor(ctx, key, "")
	band.MaxRate = pr.ProposedMax
	body, err := json.Marshal(band)
	if err != nil {
		return Proposal{}, err
	}
	if err := p.store.Set(ctx, "limits:mx:"+key, string(body)); err != nil {
		return Proposal{}, fmt.Errorf("pacer: writing the promoted band: %w", err)
	}
	if err := p.store.Set(ctx, ProposalKey(key), ""); err != nil {
		return Proposal{}, err
	}

	p.mu.Lock()
	delete(p.mx, key) // the next request re-reads; no restart needed
	p.mu.Unlock()
	return pr, nil
}

// Rate reports the rate currently settled on, for tests and observability.
func (p *Pacer) Rate(mxHost string) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.mx[PaceKey(mxHost)]
	if !ok {
		return 0, false
	}
	return st.rate, true
}

// State reports the pacer state for an MX.
func (p *Pacer) State(mxHost string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.mx[PaceKey(mxHost)]; ok {
		return st.state
	}
	return ""
}

func (p *Pacer) stateFor(ctx context.Context, key, domain string) *mxState {
	p.mu.Lock()
	if st, ok := p.mx[key]; ok {
		st.lastUsed = time.Now()
		p.mu.Unlock()
		return st
	}
	p.mu.Unlock()

	band := p.bandFor(ctx, key, domain)
	start := band.MaxRate
	// A rate persisted by an earlier run may only ever *lower* the start.
	// Backing off is a measurement; a quiet hour below the ceiling is not
	// evidence that the ceiling moved.
	if saved, ok := p.savedRate(ctx, key); ok && saved < start {
		start = max(band.MinRate, saved)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.mx[key]; ok { // another goroutine won the race
		st.lastUsed = time.Now()
		return st
	}
	p.evictLocked()
	st := &mxState{band: band, rate: start, conc: band.MinConc, state: StateProbing, lastUsed: time.Now()}
	if until, ok := p.savedPause(ctx, key); ok {
		st.pausedUntil = until
		if time.Now().Before(until) {
			st.state = StatePaused
		}
	}
	p.mx[key] = st
	return st
}

func (p *Pacer) savedRate(ctx context.Context, key string) (float64, bool) {
	raw, ok, err := p.store.Get(ctx, "rt:mx:"+key+":rate")
	if err != nil || !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func (p *Pacer) savedPause(ctx context.Context, key string) (time.Time, bool) {
	raw, ok, err := p.store.Get(ctx, "rt:mx:"+key+":pause_until")
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
func (p *Pacer) persist(ctx context.Context, key string, st mxState) {
	base := "rt:mx:" + key + ":"
	_ = p.store.Set(ctx, base+"rate", strconv.FormatFloat(st.rate, 'f', -1, 64))
	_ = p.store.Set(ctx, base+"conc", strconv.Itoa(st.conc))
	_ = p.store.Set(ctx, base+"state", st.state)
	if !st.pausedUntil.IsZero() {
		_ = p.store.Set(ctx, base+"pause_until", strconv.FormatInt(st.pausedUntil.Unix(), 10))
	}
}

// PauseKey stands a pace key down until the given time, whatever the AIMD loop
// thinks of it (plan 032): the evidence is refusals of us, which never move the
// loop (invariant 6), so the pause has to be set from outside it. It is written
// to the same `rt:mx:<key>:pause_until` the loop persists, so a restart keeps it
// and Acquire/AcquireSession refuse with an exact retry hint until it passes.
func (p *Pacer) PauseKey(ctx context.Context, mxHost string, until time.Time, reason string) error {
	key := PaceKey(mxHost)
	p.mu.Lock()
	if st, ok := p.mx[key]; ok {
		st.pausedUntil = until
		st.state = StatePaused
	}
	p.mu.Unlock()
	if p.metrics != nil {
		p.metrics.Pause(key)
	}
	_ = reason // carried by the caller's log line; the pacer keeps no prose
	return p.store.Set(ctx, "rt:mx:"+key+":pause_until", strconv.FormatInt(until.Unix(), 10))
}

// ResumeKey lifts a pause early — the operator's decision, never the loop's.
func (p *Pacer) ResumeKey(ctx context.Context, mxHost string) error {
	key := PaceKey(mxHost)
	p.mu.Lock()
	if st, ok := p.mx[key]; ok {
		st.pausedUntil = time.Time{}
		st.state = StateSteady
	}
	p.mu.Unlock()
	return p.store.Set(ctx, "rt:mx:"+key+":pause_until", "0")
}
