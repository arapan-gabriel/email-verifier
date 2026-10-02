// Package standdown pauses a pace key when refusals of us pile up in it (plan 032).
//
// A refusal of us — `ClassPolicy` — never moves the pacer (invariant 6), and on
// its own it never should: one strict or hostile server must not be able to stand
// anything down. But several *distinct* hosts of one receiving system refusing us
// inside a short window is the shape a listing takes, and continuing to knock
// keeps the listing alive. This package counts those hosts per pace key in the
// central Redis, beside the bucket (`rt:mx:<key>:refusals`), so two nodes add up
// (invariant 4), and pauses the key through the pacer when:
//
//   - rule A — a single refusal names a blocklist or a zone; or
//   - rule B — Hosts distinct hosts of the key refused us within Window.
//
// A lone host is its own pace key, so it can only ever pause itself. Replies that
// are about the recipient's configuration rather than our reputation (Microsoft's
// `5.4.1`, TLS demands, relay refusals, full mailboxes) are not counted at all.
//
// Redis failing is not a reason to invent state: the refusal goes uncounted and
// nothing is paused. The probe path itself already fails closed on the same Redis.
package standdown

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
)

// Store is the Redis subset this needs.
type Store interface {
	Do(ctx context.Context, args ...string) (any, error)
}

// Pauser stands a key down and lifts it again (the pacer).
type Pauser interface {
	PauseKey(ctx context.Context, mxHost string, until time.Time, reason string) error
	ResumeKey(ctx context.Context, mxHost string) error
}

// Recorder counts refusals of us per key. Optional.
type Recorder interface {
	RefusalOfUs(key string)
}

// Logger receives one line per stand-down and resume. Optional.
type Logger func(msg string, args ...any)

// Options configures the thresholds. Zero values take the defaults.
type Options struct {
	Hosts  int
	Window time.Duration
	Pause  time.Duration
	Now    func() time.Time
	Log    Logger
}

// benignRe matches policy replies that are about the receiving side's setup, not
// our reputation. Mirrors Data Scout's warm-up runner (`BENIGN_POLICY_RE`), with
// the relay refusals plan 033 makes possible.
var benignRe = regexp.MustCompile(`5\.4\.1|access denied|5\.7\.4|5\.4\.4|5\.7\.64|` +
	`starttls|tls connection is required|tls is required|tls required|requires tls|` +
	`session encryption is required|encryption is required|encryption required|` +
	`encryption needed|requires encryption|tls encryption|must use tls|tls version|` +
	`relay access denied|relaying denied|unable to relay|not permitted to relay|` +
	`exceeded storage allocation|quota exceeded|mailbox full|mailbox is full|over quota`)

// blocklistRe matches a refusal that names a list, a zone or our standing on one
// — rule A. `\blisted\b` and not `listed`: postgrey's "Greylisted" is a deferral.
var blocklistRe = regexp.MustCompile(`blocked using|block ?list|black ?list|\blisted\b|` +
	`spamhaus|abusix|spamcop|barracudacentral|uceprotect|dnsbl|\brbl\b|rbl\.|` +
	`magicspam|spam-source|\(s3150\)|\(s3140\)|poor reputation|bad reputation`)

var addressRe = regexp.MustCompile(`[^\s<>@]+@[^\s<>@]+`)

// Guard counts and decides. Safe for concurrent use.
type Guard struct {
	store  Store
	pauser Pauser
	rec    Recorder
	opts   Options

	mu    sync.Mutex
	stood map[string]time.Time // key -> paused until, by this rule
}

// New returns a Guard. A nil Guard is valid and does nothing.
func New(store Store, pauser Pauser, rec Recorder, opts Options) *Guard {
	if opts.Hosts < 2 {
		opts.Hosts = 3
	}
	if opts.Window <= 0 {
		opts.Window = 30 * time.Minute
	}
	if opts.Pause <= 0 {
		opts.Pause = 6 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Guard{store: store, pauser: pauser, rec: rec, opts: opts, stood: map[string]time.Time{}}
}

// RefusalsKey is where a key's refusing hosts are kept.
func RefusalsKey(key string) string { return "rt:mx:" + key + ":refusals" }

// Benign reports whether a policy reply is excluded from counting.
func Benign(reply string) bool { return benignRe.MatchString(strings.ToLower(reply)) }

// NamesBlocklist reports whether a reply is rule A.
func NamesBlocklist(reply string) bool { return blocklistRe.MatchString(strings.ToLower(reply)) }

// Observe takes one refusal of us — a `ClassPolicy` reply from mxHost — and
// returns the rule that stood its key down, or "" when nothing changed.
func (g *Guard) Observe(ctx context.Context, mxHost, reply string) string {
	if g == nil || mxHost == "" || Benign(reply) {
		return ""
	}
	key := pacer.PaceKey(mxHost)
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(mxHost), "."))
	if g.rec != nil {
		g.rec.RefusalOfUs(key)
	}
	now := g.opts.Now()

	g.mu.Lock()
	if until, ok := g.stood[key]; ok && now.Before(until) {
		g.mu.Unlock()
		return "" // already down; more refusals change nothing
	}
	g.mu.Unlock()

	hosts, err := g.count(ctx, key, host, now)
	if err != nil {
		return "" // no evidence store, no invented verdict
	}

	rule := ""
	switch {
	case NamesBlocklist(reply):
		rule = "A"
	case hosts >= g.opts.Hosts:
		rule = "B"
	default:
		return ""
	}

	until := now.Add(g.opts.Pause)
	if err := g.pauser.PauseKey(ctx, key, until, "refusals of us: rule "+rule); err != nil {
		return ""
	}
	g.mu.Lock()
	g.stood[key] = until
	g.mu.Unlock()
	if g.opts.Log != nil {
		g.opts.Log("stood_down", "pace_key", key, "rule", rule, "hosts", hosts,
			"until", until.UTC().Format(time.RFC3339), "trigger_host", host,
			"reply", addressRe.ReplaceAllString(reply, "<redacted>"))
	}
	return rule
}

// count records host as a refuser of key now and returns how many distinct
// hosts refused within the window. Four commands; a member re-added just has
// its timestamp moved.
func (g *Guard) count(ctx context.Context, key, host string, now time.Time) (int, error) {
	rk := RefusalsKey(key)
	nowMs := now.UnixMilli()
	cutoff := nowMs - g.opts.Window.Milliseconds()
	if _, err := g.store.Do(ctx, "ZADD", rk, strconv.FormatInt(nowMs, 10), host); err != nil {
		return 0, err
	}
	if _, err := g.store.Do(ctx, "ZREMRANGEBYSCORE", rk, "-inf", "("+strconv.FormatInt(cutoff, 10)); err != nil {
		return 0, err
	}
	n, err := g.store.Do(ctx, "ZCARD", rk)
	if err != nil {
		return 0, err
	}
	_, _ = g.store.Do(ctx, "PEXPIRE", rk, strconv.FormatInt(2*g.opts.Window.Milliseconds(), 10))
	switch v := n.(type) {
	case int64:
		return int(v), nil
	case int:
		return v, nil
	}
	return 0, nil
}

// Resume lifts a key's stand-down early and forgets its refusers.
func (g *Guard) Resume(ctx context.Context, mxHost string) error {
	if g == nil {
		return nil
	}
	key := pacer.PaceKey(mxHost)
	if err := g.pauser.ResumeKey(ctx, key); err != nil {
		return err
	}
	_, _ = g.store.Do(ctx, "DEL", RefusalsKey(key))
	g.mu.Lock()
	delete(g.stood, key)
	g.mu.Unlock()
	if g.opts.Log != nil {
		g.opts.Log("stand_down_resumed", "pace_key", key)
	}
	return nil
}

// StoodDown reports every key this guard has stood down and whether it still
// is — the source of the `verify_key_stood_down` gauge. A key that came back on
// its own reads false until the process restarts, so the alert clears.
func (g *Guard) StoodDown() map[string]bool {
	out := map[string]bool{}
	if g == nil {
		return out
	}
	now := g.opts.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, until := range g.stood {
		out[k] = now.Before(until)
	}
	return out
}
