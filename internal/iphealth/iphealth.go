// Package iphealth watches whether the sending IP is still usable.
//
// Automatically standing the node down is the point of this package, and also
// its danger: a false positive here is a self-inflicted outage. Everything
// below is shaped by three ways to get one, all measured on our own IP rather
// than imagined —
//
//   - a DNSBL query through a stub resolver answers "listed" for every zone;
//   - UCEPROTECT L3 lists the whole ASN, so our IP is on it while Spamhaus is
//     clean and both Gmail and Microsoft accept the session;
//   - one server answering 5.7.x is that server's opinion, not the IP's health.
//
// So: checking is opt-in and self-testing, only actionable zones count, and a
// listing pauses while an inference only alerts.
package iphealth

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultZones are lists that publish a fact about one address and that a
// delisting request can clear.
//
// UCEPROTECT L3 is deliberately absent. It lists an entire ASN: on 2026-08-28
// our IP was on it because AS16276 is, while Spamhaus ZEN, SpamCop and
// UCEPROTECT L1/L2 were clean and both Gmail and Microsoft accepted the
// session. Nothing we can do clears it, so acting on it would be a permanent
// pause for a condition no provider we tested enforces.
var DefaultZones = []string{"zen.spamhaus.org", "bl.spamcop.net"}

// LookupFunc resolves a DNSBL query name to addresses. Declared here, in the
// consumer, so tests drive it without DNS.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// Store is the Redis subset this package needs.
type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, val string) error
}

// Recorder receives the listing gauge.
type Recorder interface {
	IPListed(ip, list string, listed bool)
}

// Key returns the health key for a sending IP.
func Key(ip string) string { return "ip:health:" + ip }

// Options configures a Health.
type Options struct {
	// IP is the sending address these checks are about.
	IP string
	// Zones to query. Empty means DefaultZones.
	Zones []string
	// Lookup performs the query. **Nil disables checking entirely** — there is
	// no fallback to the process resolver, because on the deployed node that is
	// a stub and a stub answers "listed" to everything.
	Lookup LookupFunc
	// Interval between rounds.
	Interval time.Duration
	Store    Store
	Metrics  Recorder
}

// Health tracks the sending IP's standing. Safe for concurrent use.
type Health struct {
	opts    Options
	zones   []string
	mu      sync.Mutex
	burned  bool
	reason  string
	trusted bool
	tested  bool
	// policyHosts are the distinct MX hosts that have refused our client
	// recently. One is that host's opinion; several is a signal about the IP.
	policyHosts map[string]time.Time
}

// New returns a Health. It performs no I/O.
func New(opts Options) *Health {
	zones := opts.Zones
	if len(zones) == 0 {
		zones = DefaultZones
	}
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Minute
	}
	return &Health{opts: opts, zones: zones, policyHosts: map[string]time.Time{}}
}

// Enabled reports whether DNSBL checking will run at all.
func (h *Health) Enabled() bool { return h != nil && h.opts.Lookup != nil && h.opts.IP != "" }

// Burned reports whether probing should stop, and why.
//
// It reads the in-memory verdict rather than Redis: this is consulted on every
// probe, and a round trip per probe to learn something that changes every
// fifteen minutes would be waste on the hot path.
func (h *Health) Burned() (bool, string) {
	if h == nil {
		return false, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.burned, h.reason
}

// Resume clears a pause without a redeploy. The next round re-evaluates, so
// this is an override for a wrong verdict, not a way to ignore a real one.
func (h *Health) Resume(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.burned, h.reason = false, ""
	h.mu.Unlock()
	h.persist(ctx, false, "resumed by operator")
}

// ObservePolicy records that an MX refused our client.
//
// This never pauses anything. It is an inference, and pausing on it would hand
// any MX that starts answering 5.7.x — misconfigured, hostile, or merely strict
// — a way to stand our node down. It raises the gauge an operator alerts on.
func (h *Health) ObservePolicy(mxHost string) {
	if h == nil || mxHost == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policyHosts[mxHost] = time.Now()
}

// PolicyHosts reports how many distinct MX hosts have refused our client within
// the window. One is a server's opinion; several says something about the IP.
func (h *Health) PolicyHosts(window time.Duration) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-window)
	n := 0
	for host, at := range h.policyHosts {
		if at.Before(cutoff) {
			delete(h.policyHosts, host)
			continue
		}
		n++
		_ = host
	}
	return n
}

// SelfTest establishes whether the resolver can answer DNSBL queries at all.
//
// A stub resolver answers 127.255.255.254 to every zone, which reads as "listed
// everywhere". A checker that trusted that would pause the node for a resolver
// misconfiguration. So before any real query: the zone's documented test point
// must come back listed, and its documented clean point must not.
func (h *Health) SelfTest(ctx context.Context) error {
	if !h.Enabled() {
		return fmt.Errorf("iphealth: no resolver configured for DNSBL queries")
	}
	for _, zone := range h.zones {
		listed, err := h.query(ctx, "2.0.0.127", zone)
		if err != nil {
			return fmt.Errorf("iphealth: %s test point unreachable: %w", zone, err)
		}
		if !listed {
			return fmt.Errorf("iphealth: %s did not list its own test point; the resolver cannot query it", zone)
		}
		clean, err := h.query(ctx, "1.0.0.127", zone)
		if err != nil {
			return fmt.Errorf("iphealth: %s clean point unreachable: %w", zone, err)
		}
		if clean {
			return fmt.Errorf("iphealth: %s listed its own clean point; the resolver answers everything (a stub)", zone)
		}
	}
	h.mu.Lock()
	h.trusted, h.tested = true, true
	h.mu.Unlock()
	return nil
}

// Report is the outcome of one round.
type Report struct {
	Listed map[string]bool // zone -> listed
	Burned bool
	Reason string
}

// Check runs one round. It pauses the node only on a confirmed listing from a
// resolver that passed the self-test.
func (h *Health) Check(ctx context.Context) (Report, error) {
	if !h.Enabled() {
		return Report{}, fmt.Errorf("iphealth: checking is disabled")
	}
	h.mu.Lock()
	trusted := h.trusted
	h.mu.Unlock()
	if !trusted {
		return Report{}, fmt.Errorf("iphealth: resolver has not passed the self-test; refusing to act on its answers")
	}

	rep := Report{Listed: map[string]bool{}}
	var on []string
	for _, zone := range h.zones {
		listed, err := h.query(ctx, reverse(h.opts.IP), zone)
		if err != nil {
			// A failed query is not a listing. Treating it as one is the
			// false positive this package exists to avoid.
			continue
		}
		rep.Listed[zone] = listed
		if h.opts.Metrics != nil {
			h.opts.Metrics.IPListed(h.opts.IP, zone, listed)
		}
		if listed {
			on = append(on, zone)
		}
	}

	if len(on) > 0 {
		rep.Burned = true
		rep.Reason = "listed on " + strings.Join(on, ", ")
	}

	h.mu.Lock()
	h.burned, h.reason = rep.Burned, rep.Reason
	h.mu.Unlock()
	h.persist(ctx, rep.Burned, rep.Reason)
	return rep, nil
}

// Run checks on a ticker until the context is cancelled.
func (h *Health) Run(ctx context.Context) {
	if !h.Enabled() {
		return
	}
	t := time.NewTicker(h.opts.Interval)
	defer t.Stop()
	for {
		_, _ = h.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Health) query(ctx context.Context, prefix, zone string) (bool, error) {
	addrs, err := h.opts.Lookup(ctx, prefix+"."+zone)
	if err != nil {
		return false, err
	}
	// A listing is any 127.0.0.0/8 answer. 127.255.255.254 is the "your query
	// was refused" sentinel and is not a listing — but the self-test is what
	// actually catches a resolver returning it, because a refused resolver
	// returns it for the clean point too.
	for _, a := range addrs {
		if a.Is4() && a.As4()[0] == 127 && a.String() != "127.255.255.254" {
			return true, nil
		}
	}
	return false, nil
}

func (h *Health) persist(ctx context.Context, burned bool, reason string) {
	if h.opts.Store == nil {
		return
	}
	v := "ok"
	if burned {
		v = "burned:" + reason
	}
	_ = h.opts.Store.Set(ctx, Key(h.opts.IP), v)
}

// reverse turns 92.222.87.97 into 97.87.222.92, as DNSBL queries require.
func reverse(ip string) string {
	parts := strings.Split(ip, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}
