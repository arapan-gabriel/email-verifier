package policy

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
)

// Action is what the SMTP session should do with a verdict.
type Action string

const (
	ActionReply   Action = "reply"   // write Reply and carry on
	ActionClose   Action = "close"   // write Reply, then close (421-style)
	ActionDrop    Action = "drop"    // close hard, no reply (RST)
	ActionTimeout Action = "timeout" // never reply, let the client time out
)

// Verdict is the outcome of a policy decision.
type Verdict struct {
	Action Action
	Reply  string
	// Reason is for stats/transcript only, never sent on the wire.
	Reason string
}

func reply(s string) Verdict { return Verdict{Action: ActionReply, Reply: s} }
func code(v Verdict) int {
	if len(v.Reply) >= 3 {
		n := 0
		for i := 0; i < 3; i++ {
			c := v.Reply[i]
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// Stats is the observable truth about what the server saw. Tests assert on
// this rather than on client-side outcomes.
type Stats struct {
	Profile           string            `json:"profile"`
	Conns             int64             `json:"conns"`
	ConnsRejected     int64             `json:"conns_rejected"`
	Rcpt              int64             `json:"rcpt"`
	Accepted          int64             `json:"accepted"`
	Rejected          int64             `json:"rejected"`
	Throttled         int64             `json:"throttled"`
	TempErrors        int64             `json:"temp_errors"`
	Greylisted        int64             `json:"greylisted"`
	Drops             int64             `json:"drops"`
	Timeouts          int64             `json:"timeouts"`
	BadSequence       int64             `json:"bad_sequence"`
	MaxConcurrentSeen int               `json:"max_concurrent_seen"`
	CurrentConcurrent int               `json:"current_concurrent"`
	PeakRatePerMin    int               `json:"peak_rate_per_min"`
	Cooldowns         int64             `json:"cooldowns"`
	FirstThrottleAt   *time.Time        `json:"first_throttle_at"`
	LastThrottleAt    *time.Time        `json:"last_throttle_at"`
	CodeCounts        map[string]int64  `json:"code_counts"`
	ActiveCooldowns   map[string]string `json:"active_cooldowns"`
	ClockOffset       string            `json:"clock_offset"`
}

// Line is one transcript entry. Dir is "C" (client) or "S" (server).
type Line struct {
	TS   time.Time `json:"ts"`
	IP   string    `json:"ip"`
	Conn int64     `json:"conn"`
	Dir  string    `json:"dir"`
	Text string    `json:"text"`
}

type ring struct {
	mu   sync.Mutex
	buf  []Line
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]Line, n)} }

func (r *ring) add(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = l
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

func (r *ring) last(n int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.buf)
	}
	if n > size {
		n = size
	}
	out := make([]Line, 0, n)
	for i := size - n; i < size; i++ {
		idx := i
		if r.full {
			idx = (r.next + i) % len(r.buf)
		}
		out = append(out, r.buf[idx])
	}
	return out
}

func (r *ring) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next, r.full = 0, false
}

// ipState is per-source-IP policy state. Real MXes throttle per IP; so do we,
// which is what makes the future probe-pool routing testable.
type ipState struct {
	connTimes     []time.Time
	rcptTimes     []time.Time
	rcptMinute    []time.Time
	active        int
	cooldownUntil time.Time
	greylist      map[string]time.Time
}

// Engine holds the mutable policy state for one profile.
type Engine struct {
	mu     sync.Mutex
	prof   Profile
	clk    clock.Clock
	ips    map[string]*ipState
	st     Stats
	rng    *rand.Rand
	tr     *ring
	connNo int64
}

func NewEngine(p *Profile, clk clock.Clock) *Engine {
	e := &Engine{
		prof: *p,
		clk:  clk,
		ips:  map[string]*ipState{},
		rng:  rand.New(rand.NewSource(p.Chaos.Seed)),
		tr:   newRing(2000),
	}
	e.st.Profile = p.Name
	e.st.CodeCounts = map[string]int64{}
	return e
}

func (e *Engine) Name() string { return e.prof.Name }

func (e *Engine) Profile() Profile {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.prof
}

// SetProfile hot-swaps config. Counters survive; cooldowns do not, because a
// new config with new limits should not stay punished under the old ones.
func (e *Engine) SetProfile(p *Profile) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Listen addresses are bound at startup and cannot change at runtime.
	p.Listen = e.prof.Listen
	e.prof = *p
	e.rng = rand.New(rand.NewSource(p.Chaos.Seed))
	for _, s := range e.ips {
		s.cooldownUntil = time.Time{}
	}
}

func (e *Engine) SetChaos(c Chaos) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.TempErrorReply == "" {
		c.TempErrorReply = e.prof.Chaos.TempErrorReply
	}
	if c.Seed == 0 {
		c.Seed = e.prof.Chaos.Seed
	}
	e.prof.Chaos = c
	e.rng = rand.New(rand.NewSource(c.Seed))
}

// Reset zeroes counters, cooldowns, greylist entries and the transcript.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ips = map[string]*ipState{}
	active := e.st.CurrentConcurrent
	e.st = Stats{Profile: e.prof.Name, CodeCounts: map[string]int64{}, CurrentConcurrent: active}
	e.rng = rand.New(rand.NewSource(e.prof.Chaos.Seed))
	e.tr.reset()
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.st
	out.CodeCounts = map[string]int64{}
	for k, v := range e.st.CodeCounts {
		out.CodeCounts[k] = v
	}
	now := e.clk.Now()
	out.ActiveCooldowns = map[string]string{}
	for ip, s := range e.ips {
		if now.Before(s.cooldownUntil) {
			out.ActiveCooldowns[ip] = s.cooldownUntil.Sub(now).Round(time.Second).String()
		}
	}
	out.ClockOffset = e.clk.Offset().String()
	return out
}

func (e *Engine) Transcript(n int) []Line { return e.tr.last(n) }

func (e *Engine) Log(ip string, conn int64, dir, text string) {
	e.tr.add(Line{TS: e.clk.Now(), IP: ip, Conn: conn, Dir: dir, Text: text})
}

func (e *Engine) NextConnID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.connNo++
	return e.connNo
}

// prune drops timestamps older than the window and returns the kept slice.
func prune(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	i := 0
	for ; i < len(ts); i++ {
		if ts[i].After(cut) {
			break
		}
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}

func (e *Engine) state(ip string) *ipState {
	s := e.ips[ip]
	if s == nil {
		s = &ipState{greylist: map[string]time.Time{}}
		e.ips[ip] = s
	}
	return s
}

func (e *Engine) startCooldown(s *ipState, now time.Time) {
	d := e.prof.Limits.CooldownAfterExceed.D()
	if d <= 0 {
		return
	}
	if now.Add(d).After(s.cooldownUntil) {
		s.cooldownUntil = now.Add(d)
		e.st.Cooldowns++
	}
}

func (e *Engine) countThrottle(now time.Time) {
	e.st.Throttled++
	if e.st.FirstThrottleAt == nil {
		t := now
		e.st.FirstThrottleAt = &t
	}
	t := now
	e.st.LastThrottleAt = &t
}

func (e *Engine) countCode(v Verdict) {
	if c := code(v); c > 0 {
		e.st.CodeCounts[fmt.Sprint(c)]++
	}
}

// OnConnect decides whether a new connection is allowed. The caller must call
// OnDisconnect exactly once for every call that returns ok, including the
// rejected ones — the connection still existed.
func (e *Engine) OnConnect(ip string) (Verdict, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	s := e.state(ip)

	e.st.Conns++
	s.connTimes = append(s.connTimes, now)

	if now.Before(s.cooldownUntil) {
		e.st.ConnsRejected++
		e.countThrottle(now)
		v := Verdict{Action: ActionClose, Reply: e.prof.Limits.ConnRate.OnExceed, Reason: "cooldown"}
		if v.Reply == "" {
			v.Reply = "421 4.7.0 Service temporarily unavailable, try again later"
		}
		e.countCode(v)
		return v, false
	}

	if l := e.prof.Limits.MaxConcurrentConns; l > 0 && s.active >= l {
		e.st.ConnsRejected++
		e.countThrottle(now)
		e.startCooldown(s, now)
		v := Verdict{Action: ActionClose, Reply: e.prof.Limits.TooManyConns, Reason: "max_concurrent"}
		e.countCode(v)
		return v, false
	}

	if r := e.prof.Limits.ConnRate; r.enabled() {
		s.connTimes = prune(s.connTimes, now, r.Window.D())
		if len(s.connTimes) > r.Count {
			e.st.ConnsRejected++
			e.countThrottle(now)
			e.startCooldown(s, now)
			v := Verdict{Action: ActionClose, Reply: r.OnExceed, Reason: "conn_rate"}
			e.countCode(v)
			return v, false
		}
	}

	s.active++
	e.st.CurrentConcurrent++
	if e.st.CurrentConcurrent > e.st.MaxConcurrentSeen {
		e.st.MaxConcurrentSeen = e.st.CurrentConcurrent
	}
	return reply(e.prof.Banner), true
}

func (e *Engine) OnDisconnect(ip string, admitted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !admitted {
		return
	}
	s := e.state(ip)
	if s.active > 0 {
		s.active--
	}
	if e.st.CurrentConcurrent > 0 {
		e.st.CurrentConcurrent--
	}
}

// OnRcpt is the decision that matters: everything a verifier learns comes from
// this reply.
func (e *Engine) OnRcpt(ip, addr string, perConn int) Verdict {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	s := e.state(ip)
	e.st.Rcpt++
	s.rcptMinute = append(s.rcptMinute, now)
	s.rcptMinute = prune(s.rcptMinute, now, time.Minute)
	if n := len(s.rcptMinute); n > e.st.PeakRatePerMin {
		e.st.PeakRatePerMin = n
	}

	throttle := func(replyText, reason string) Verdict {
		e.countThrottle(now)
		e.startCooldown(s, now)
		v := Verdict{Action: ActionClose, Reply: replyText, Reason: reason}
		e.countCode(v)
		return v
	}

	if now.Before(s.cooldownUntil) {
		r := e.prof.Limits.RcptRate.OnExceed
		if r == "" {
			r = "421 4.7.0 Service temporarily unavailable, try again later"
		}
		return throttle(r, "cooldown")
	}

	if l := e.prof.Limits.RcptPerConn; l > 0 && perConn > l {
		v := Verdict{Action: ActionReply, Reply: "452 4.5.3 Too many recipients", Reason: "rcpt_per_conn"}
		e.st.TempErrors++
		e.countCode(v)
		return v
	}

	if r := e.prof.Limits.RcptRate; r.enabled() {
		s.rcptTimes = append(s.rcptTimes, now)
		s.rcptTimes = prune(s.rcptTimes, now, r.Window.D())
		if len(s.rcptTimes) > r.Count {
			return throttle(r.OnExceed, "rcpt_rate")
		}
	}

	local, domain := split(addr)

	if e.prof.Chaos.DropRate > 0 && e.rng.Float64() < e.prof.Chaos.DropRate {
		e.st.Drops++
		return Verdict{Action: ActionDrop, Reason: "chaos_drop"}
	}
	if e.prof.Chaos.TempErrorRate > 0 && e.rng.Float64() < e.prof.Chaos.TempErrorRate {
		e.st.TempErrors++
		v := Verdict{Action: ActionReply, Reply: e.prof.Chaos.TempErrorReply, Reason: "chaos_temp"}
		e.countCode(v)
		return v
	}

	switch {
	case normalizeLocals(e.prof.Recipients.Drop)[local]:
		e.st.Drops++
		return Verdict{Action: ActionDrop, Reason: "recipient_drop"}
	case normalizeLocals(e.prof.Recipients.Timeout)[local]:
		e.st.Timeouts++
		return Verdict{Action: ActionTimeout, Reason: "recipient_timeout"}
	}

	if e.prof.Behaviour.Greylist {
		key := ip + "|" + strings.ToLower(addr)
		first, seen := s.greylist[key]
		if !seen {
			s.greylist[key] = now
			e.st.Greylisted++
			v := Verdict{Action: ActionReply, Reply: e.prof.Behaviour.GreylistReply, Reason: "greylist_first"}
			e.countCode(v)
			return v
		}
		if now.Sub(first) < e.prof.Behaviour.GreylistDelay.D() {
			e.st.Greylisted++
			v := Verdict{Action: ActionReply, Reply: e.prof.Behaviour.GreylistReply, Reason: "greylist_early"}
			e.countCode(v)
			return v
		}
	}

	known := normalizeLocals(e.prof.Recipients.Exists)[local]
	bounce := normalizeLocals(e.prof.Recipients.Bounce)[local]
	_ = domain

	switch {
	case bounce:
		e.st.Rejected++
		v := reply(rejectFor(e.prof.Behaviour.RejectUnknown, addr))
		v.Reason = "bounce_list"
		e.countCode(v)
		return v
	case known || e.prof.Behaviour.CatchAll:
		e.st.Accepted++
		v := reply(e.prof.Behaviour.Accept)
		v.Reason = "accept"
		if !known {
			v.Reason = "catch_all"
		}
		e.countCode(v)
		return v
	default:
		e.st.Rejected++
		v := reply(rejectFor(e.prof.Behaviour.RejectUnknown, addr))
		v.Reason = "unknown"
		e.countCode(v)
		return v
	}
}

func (e *Engine) CountBadSequence() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.BadSequence++
	e.st.CodeCounts["503"]++
}

// TarpitBanner and TarpitRcpt are read under lock so /profiles can change them
// while connections are in flight.
func (e *Engine) TarpitBanner() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.prof.Behaviour.TarpitBanner.D()
}

func (e *Engine) TarpitRcpt() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.prof.Behaviour.TarpitRcpt.D()
}

func rejectFor(tmpl, addr string) string {
	if strings.Contains(tmpl, "%s") {
		return fmt.Sprintf(tmpl, addr)
	}
	return tmpl
}

func split(addr string) (local, domain string) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	i := strings.LastIndex(addr, "@")
	if i < 0 {
		return addr, ""
	}
	return addr[:i], addr[i+1:]
}
