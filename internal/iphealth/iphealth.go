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
	"errors"
	"fmt"
	"net"
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
//
// Abusix Mail Intelligence is absent for a different reason: it belongs here on
// the merits — it refused our IP on 2026-09-12 while both zones below were
// clean — but its query name carries a subscription key
// (<reversed-ip>.<APIKEY>.combined.mail.abusix.zone), so a keyless default
// would fail its self-test everywhere. It is configured per deployment instead;
// see config/verifierd.yaml.
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
	// ComplaintWindow and ComplaintThreshold decide when spam complaints pause
	// *sending* (plan 015). A zero threshold disables the pause: a node with no
	// relay has no complaints to count, and a threshold nobody chose should not
	// stop mail on its own.
	ComplaintWindow    time.Duration
	ComplaintThreshold int
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
	// complaints are when someone marked our mail as spam (plan 015). Kept as
	// timestamps rather than a count so the rate can be read over a window: a
	// complaint from last month says nothing about today.
	complaints []time.Time
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

// ObserveComplaint records that a recipient marked our mail as spam.
//
// A complaint is worth far more attention than a bounce. A bounce says an
// address is wrong; a complaint says a person who could receive our mail did
// not want it, and complaint rate is the number that gets a sending IP blocked
// — usually before anything else shows it.
func (h *Health) ObserveComplaint() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complaints = append(h.complaints, time.Now())
}

// Complaints reports how many arrived within the window.
func (h *Health) Complaints(window time.Duration) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-window)
	kept := h.complaints[:0]
	for _, at := range h.complaints {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	h.complaints = kept
	return len(kept)
}

// SendingPaused reports whether outbound mail should stop while verification
// continues.
//
// The two legs are deliberately not the same switch. A blocklist entry stops
// both — it is about the address itself. A complaint spike stops only sending:
// the complaints are about *messages*, and pausing verification would not
// improve anything while costing every customer their answers.
func (h *Health) SendingPaused() (bool, string) {
	if h == nil || h.opts.ComplaintThreshold <= 0 {
		return false, ""
	}
	window := h.opts.ComplaintWindow
	if window <= 0 {
		window = 24 * time.Hour
	}
	if n := h.Complaints(window); n >= h.opts.ComplaintThreshold {
		return true, fmt.Sprintf("%d spam complaints in the last %s", n, window)
	}
	return false, ""
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

// ZoneFinding is a zone and what the self-test found about it — the reason it
// was refused, or the caveat it was kept with.
type ZoneFinding struct {
	Zone   string
	Reason error
}

// SelfTestResult is what one pass of the self-test found. Dropped zones are not
// queried again; zones in Caveats are watched normally and carry something an
// operator should know about them.
type SelfTestResult struct {
	Dropped []ZoneFinding
	Caveats []ZoneFinding
}

// unlistablePoints are addresses no honest blocklist can carry: RFC 5737
// documentation ranges, which are not routable, so no mail has ever come from
// them. They are the second opinion when a zone lists RFC 5782's clean point.
//
// **Two of them, not one**, because the finding that produced them is precisely
// that a real list can carry an address it should not: on 2026-09-16
// rbl.your-server.de listed 127.0.0.1 (TXT "Last seen 2026-09-16 08:30:03" —
// its entries come from what receiving servers report, and somebody's
// misconfigured host reported its own loopback). One bogus entry in a
// documentation range would otherwise disqualify a working zone the same way.
// A resolver that answers everything answers both.
//
// Fixed rather than random: gosec flags math/rand, a crypto source for a DNS
// label is theatre, and a fixed query is one a person can repeat by hand from
// the journal line that named it.
var unlistablePoints = []struct{ prefix, addr string }{
	{"1.2.0.192", "192.0.2.1"},
	{"1.113.0.203", "203.0.113.1"},
}

// SelfTest establishes whether the resolver can answer DNSBL queries, one zone
// at a time.
//
// A stub resolver answers 127.255.255.254 to every zone, which reads as "listed
// everywhere". A checker that trusted that would pause the node for a resolver
// misconfiguration. So before any real query: the zone's documented test point
// must come back listed, and its documented clean point must not.
//
// **Per zone, not all-or-nothing.** This used to return on the first failing
// zone, and the caller disabled blocklist checking entirely on any error — so
// adding a third zone that could not answer (an expired subscription key, a
// rate-limited reply, a rename) switched off the two that worked. Coverage
// could then only be bought by risking the coverage already there, which is the
// wrong trade for a package whose whole point is that the node knows its own
// standing. Zones that pass are kept, zones that fail are returned for the
// caller to log, and an error comes back only when **none** passed — the
// original "this resolver cannot do DNSBL at all" case.
//
// h.zones is narrowed to the survivors, so Check never queries a zone known to
// be unanswerable and ip_health_listed carries a series only for zones actually
// checked. A zone missing from that metric therefore means "not covered", which
// is a different fact from "covered and clean" and the one worth being able to
// read: on 2026-09-12 the node reported burned: false while listed on a zone it
// was not watching.
func (h *Health) SelfTest(ctx context.Context) (SelfTestResult, error) {
	var result SelfTestResult
	if !h.Enabled() {
		return result, fmt.Errorf("iphealth: no resolver configured for DNSBL queries")
	}
	var kept []string
	for _, zone := range h.zones {
		caveat, err := h.selfTestZone(ctx, zone)
		if err != nil {
			result.Dropped = append(result.Dropped, ZoneFinding{Zone: zone, Reason: err})
			continue
		}
		if caveat != nil {
			result.Caveats = append(result.Caveats, ZoneFinding{Zone: zone, Reason: caveat})
		}
		kept = append(kept, zone)
	}
	if len(kept) == 0 {
		// Every zone failed. Report the first reason rather than a bare count:
		// with one configured zone this is the old message verbatim, and with
		// several the first is as good a witness as any to a broken resolver.
		return result, fmt.Errorf("iphealth: no zone passed its self-test: %w", result.Dropped[0].Reason)
	}
	h.mu.Lock()
	h.zones = kept
	h.trusted, h.tested = true, true
	h.mu.Unlock()
	return result, nil
}

// selfTestZone probes one zone's documented test and clean points. It returns
// a caveat — a zone worth keeping and worth saying something about — or an
// error, which means the zone is not trusted at all.
//
// Every reason names the zone redacted: the caller logs it, and the case that
// produces one for a keyed zone is precisely a key that stopped working.
func (h *Health) selfTestZone(ctx context.Context, zone string) (caveat, err error) {
	reported := RedactZone(zone)
	listed, err := h.query(ctx, "2.0.0.127", zone)
	if err != nil {
		return nil, fmt.Errorf("%s test point unreachable: %w", reported, err)
	}
	if !listed {
		return nil, fmt.Errorf("%s did not list its own test point; the resolver cannot query it", reported)
	}
	clean, err := h.query(ctx, "1.0.0.127", zone)
	if err != nil {
		return nil, fmt.Errorf("%s clean point unreachable: %w", reported, err)
	}
	if !clean {
		return nil, nil
	}
	// RFC 5782 says a list must never carry 127.0.0.1, so this is either a
	// resolver answering everything — the outage this test exists to prevent —
	// or a list carrying an entry it should not. Only the first must cost the
	// zone, and the two are told apart by asking about addresses no list can
	// legitimately hold.
	return h.secondOpinion(ctx, zone, reported)
}

// secondOpinion decides what a listed clean point means. Both unlistable points
// listed is a resolver answering everything; either one clean is a real list
// with junk in it, kept with a caveat. If neither answered clean and something
// errored, the opinion could not be taken — and a zone whose answers cannot be
// read is what this test refuses.
func (h *Health) secondOpinion(ctx context.Context, zone, reported string) (caveat, err error) {
	var failed error
	for _, point := range unlistablePoints {
		junk, qerr := h.query(ctx, point.prefix, zone)
		if qerr != nil {
			failed = qerr
			continue
		}
		if junk {
			continue
		}
		return fmt.Errorf(
			"%s lists 127.0.0.1, which RFC 5782 reserves as its clean point; kept because %s is not listed, "+
				"so this is a list carrying a bad entry rather than a resolver answering everything",
			reported, point.addr), nil
	}
	if failed != nil {
		return nil, fmt.Errorf("%s listed its own clean point and the second opinion is unreachable: %w", reported, failed)
	}
	return nil, fmt.Errorf(
		"%s listed its own clean point, and %s and %s with it; the resolver answers everything (a stub)",
		reported, unlistablePoints[0].addr, unlistablePoints[1].addr)
}

// Zones reports the zones actually being checked — after SelfTest, the
// survivors. The caller logs these rather than the configured list, because
// "what we watch" and "what we were asked to watch" stopped being the same
// thing the moment one zone could drop out.
func (h *Health) Zones() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.zones...)
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
	for _, zone := range h.Zones() {
		listed, err := h.query(ctx, reverse(h.opts.IP), zone)
		if err != nil {
			// A failed query is not a listing. Treating it as one is the
			// false positive this package exists to avoid.
			continue
		}
		// Reported redacted, queried raw: a keyed zone carries a subscription
		// key in its own name, and all three of these are read by a person —
		// the metric, the report, and the `reason` that reaches an email.
		reported := RedactZone(zone)
		rep.Listed[reported] = listed
		if h.opts.Metrics != nil {
			h.opts.Metrics.IPListed(h.opts.IP, reported, listed)
		}
		if listed {
			on = append(on, reported)
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
		// NXDOMAIN is how a DNSBL says "not listed" — the overwhelmingly
		// common answer. Go reports it as an error, and treating that as an
		// unreachable zone makes every clean zone look broken: the self-test
		// then fails permanently and blocklist checking never runs at all.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, nil
		}
		// Redacted here rather than at each caller: the error names the whole
		// query, and a keyed zone's key is part of that name.
		return false, redactError(err, zone)
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
