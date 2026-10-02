// Package metrics exposes what this service is doing, in Prometheus text
// format, without a Prometheus client library.
//
// The library pulls protobuf, procfs and expfmt into a repository that has one
// dependency. The exposition format is text, the metric set here is fixed and
// small, and this repository already hand-rolls its RESP client and its SMTP
// state machine for the same reason.
package metrics

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// buckets are the upper bounds for request duration, in seconds. They straddle
// what a probe actually costs: a fast rejection is milliseconds, a normal
// session is under a second, and anything past ten is a server tarpitting us.
var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// domainBuckets are the upper bounds for domains asked per SMTP session (plan
// 033). 1 is every ungrouped session; the rest show how much grouping happens.
var domainBuckets = []int{1, 2, 3, 5, 10, 20, 50}

// MXState is one row of what the pacer is currently tracking.
type MXState struct {
	Host string
	Rate float64
	// MaxRate is the band's ceiling. The gauges do not report it, but the
	// operator's band view needs it and this is already the snapshot.
	MaxRate float64
	Conc    int
	State   string
	// Inflight is how many session leases this process holds for the key
	// (plan 028). The authoritative count is the central lease set.
	Inflight int
}

// Pacer is the gauge source. Gauges are pulled at scrape time rather than
// pushed, because the pacer owns that state and mirroring it would give two
// answers that can disagree.
//
// Declared here, in the consumer (ENGINEERING-STANDARDS §2).
type Pacer interface {
	Snapshot() []MXState
}

// Registry holds every metric this service reports. Safe for concurrent use.
type Registry struct {
	mu sync.Mutex

	results     map[string]uint64 // class
	replies     map[[2]string]uint64
	blocked     map[string]uint64 // reason
	catchAll    map[string]uint64 // catch-all probe outcome (plan 029)
	leaseWaits  map[string]uint64 // session lease outcome (plan 028)
	tlsSessions map[string]uint64 // STARTTLS outcome per session (plan 031)
	pauses      map[string]uint64 // mx host
	refusals    map[string]uint64 // refusals of us per pace key (plan 032)
	fallbacks   map[string]uint64 // multi-domain fallbacks per family (plan 033)
	domCounts   []uint64          // domains-per-session histogram, cumulative, plus +Inf
	domSum      uint64
	listed      map[[2]string]bool // {ip, list} -> listed
	sent        map[string]uint64  // relay delivery outcome (plan 014)
	qDepth      [3]int             // relay queue: ready, later, dead
	complaints  uint64             // spam complaints reported by the caller (plan 015)
	counts      []uint64           // duration histogram, one per bucket plus +Inf
	sum         float64
	observed    uint64

	pacer       Pacer
	standDown   StandDownSource
	policyHosts func() int
}

// StandDownSource reports the keys stood down for refusals of us (plan 032).
type StandDownSource interface {
	StoodDown() map[string]bool
}

// New returns a Registry. pacer may be nil, in which case the per-MX gauges are
// simply absent rather than wrong.
func New(pacer Pacer) *Registry {
	return &Registry{
		results:     map[string]uint64{},
		replies:     map[[2]string]uint64{},
		blocked:     map[string]uint64{},
		catchAll:    map[string]uint64{},
		leaseWaits:  map[string]uint64{},
		tlsSessions: map[string]uint64{},
		pauses:      map[string]uint64{},
		refusals:    map[string]uint64{},
		fallbacks:   map[string]uint64{},
		domCounts:   make([]uint64, len(domainBuckets)+1),
		listed:      map[[2]string]bool{},
		sent:        map[string]uint64{},
		counts:      make([]uint64, len(buckets)+1),
		pacer:       pacer,
	}
}

// SetPacer attaches the gauge source after construction. The pacer needs the
// registry to count pauses and the registry needs the pacer for its gauges, so
// one of the two has to be wired second.
func (r *Registry) SetPacer(p Pacer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pacer = p
}

// SetStandDown attaches the stand-down gauge source (plan 032).
func (r *Registry) SetStandDown(s StandDownSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.standDown = s
}

// SetPolicyHosts attaches the distinct-refusing-hosts gauge (ip health).
func (r *Registry) SetPolicyHosts(f func() int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policyHosts = f
}

// RefusalOfUs counts one refusal of us. A family key is its own label; every
// lone host shares one, so the series cannot grow with request input.
func (r *Registry) RefusalOfUs(key string) {
	label := "host"
	if strings.HasPrefix(key, "@") {
		label = key
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals[label]++
}

// Result records one address answered, by its class.
func (r *Registry) Result(class string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results[class]++
}

// Reply records one SMTP reply read, by code and classification.
func (r *Registry) Reply(code int, class string) {
	if code == 0 {
		return // no reply was read; Blocked covers that case
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies[[2]string{strconv.Itoa(code), class}]++
}

// Blocked records a probe this service declined to send, by reason.
//
// The reasons are bounded on purpose: guarded, no_budget, paused, policy_stop.
// They are what an operator alerts on, and they mean different things —
// no_budget is an incident, paused is normal operation.
func (r *Registry) Blocked(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked[reason]++
}

// CatchAllProbes records how one domain's catch-all question was settled (plan
// 029). The outcomes are bounded: skipped_no_accept, clean_after_1,
// catch_all_after_2, randomiser_after_2, audit_full, unanswered.
func (r *Registry) CatchAllProbes(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.catchAll[outcome]++
}

// LeaseWait records how a session lease was obtained (plan 028): immediate,
// granted_after_wait or timed_out. Bounded.
func (r *Registry) LeaseWait(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseWaits[outcome]++
}

// TLSSession records how one SMTP session was encrypted (plan 031): none,
// verified, unverified, fallback, skipped or failed. Bounded.
func (r *Registry) TLSSession(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tlsSessions[outcome]++
}

// SessionDomains records how many domains one SMTP session asked about (plan
// 033).
func (r *Registry) SessionDomains(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, b := range domainBuckets {
		if n <= b {
			r.domCounts[i]++
		}
	}
	r.domCounts[len(domainBuckets)]++
	r.domSum += uint64(max(n, 0))
}

// MultiDomainFallback records a family switched to one domain per session on
// this node after a relay refusal (plan 033). Only cleared families can fall
// back, so the label is bounded by families.json.
func (r *Registry) MultiDomainFallback(family string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallbacks[family]++
}

// Pause records the pacer standing an MX down.
func (r *Registry) Pause(mxHost string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauses[mxHost]++
}

// IPListed records whether a sending IP is on a given blocklist. It is a gauge
// rather than a counter: what matters is the current standing, not how many
// times we have asked.
func (r *Registry) IPListed(ip, list string, listed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed[[2]string{ip, list}] = listed
}

// Sent records one delivery attempt by outcome (plan 014).
//
// The outcomes are bounded and mean different things to an operator: delivered
// and rejected are answers from a server, deferred is normal, and suppressed,
// no_budget and no_mx are this service declining to send rather than anyone
// refusing us.
func (r *Registry) Sent(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent[outcome]++
}

// Complaint records one spam complaint, reported by the caller — this service
// never receives a feedback loop itself.
//
// A counter and not a gauge on purpose: the *rate* is what matters, and a rate
// is something a scraper computes from a counter. What pauses sending is the
// count within a window, which lives in iphealth where the timestamps are.
func (r *Registry) Complaint() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.complaints++
}

// QueueDepth reports what is waiting. A gauge, not a counter: the question is
// whether anything is stuck now, and `dead` above zero always wants a person.
func (r *Registry) QueueDepth(ready, later, dead int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.qDepth = [3]int{ready, later, dead}
}

// Observe records one request's end-to-end duration in seconds.
func (r *Registry) Observe(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sum += seconds
	r.observed++
	for i, b := range buckets {
		if seconds <= b {
			r.counts[i]++
		}
	}
	r.counts[len(buckets)]++ // +Inf
}

// Render writes the exposition. Buckets are cumulative, as the format requires:
// a value in the 0.05 bucket is also in every bucket above it.
func (r *Registry) Render() string {
	r.mu.Lock()
	results := maps.Clone(r.results)
	replies := maps.Clone(r.replies)
	blocked := maps.Clone(r.blocked)
	catchAll := maps.Clone(r.catchAll)
	leaseWaits := maps.Clone(r.leaseWaits)
	tlsSessions := maps.Clone(r.tlsSessions)
	pauses := maps.Clone(r.pauses)
	refusals := maps.Clone(r.refusals)
	fallbacks := maps.Clone(r.fallbacks)
	domCounts, domSum := slices.Clone(r.domCounts), r.domSum
	standDown, policyHosts := r.standDown, r.policyHosts
	listed := maps.Clone(r.listed)
	counts := slices.Clone(r.counts)
	sum, observed := r.sum, r.observed
	pacer := r.pacer
	// The relay fields too. They were read after Unlock, and a scrape racing
	// Sent() is a concurrent map read and write — a fatal error that takes the
	// process down, not a stale number (plan 027, found under -race).
	sent := maps.Clone(r.sent)
	qDepth, complaints := r.qDepth, r.complaints
	r.mu.Unlock()

	var b strings.Builder

	counter(&b, "verify_results_total", "Addresses answered, by classification.", results, "class")
	writeReplies(&b, replies)
	counter(&b, "verify_probe_blocked_total",
		"Probes this service declined to send, by reason.", blocked, "reason")
	counter(&b, "verify_catch_all_probes_total",
		"How each domain's catch-all question was settled, by outcome.", catchAll, "outcome")
	counter(&b, "verify_lease_waits_total",
		"How session leases were obtained, by outcome (plan 028).", leaseWaits, "outcome")
	counter(&b, "verify_tls_sessions_total",
		"How SMTP sessions were encrypted, by STARTTLS outcome (plan 031).", tlsSessions, "outcome")
	counter(&b, "verify_pause_events_total",
		"Times the pacer stood an MX down after throttling at the floor of its band.", pauses, "mx_host")

	counter(&b, "verify_multi_domain_fallbacks_total",
		"Families switched to one domain per session after a relay refusal (plan 033).", fallbacks, "family")
	fmt.Fprint(&b, "# HELP verify_session_domains Domains asked about per SMTP session (plan 033).\n")
	fmt.Fprint(&b, "# TYPE verify_session_domains histogram\n")
	for i, bound := range domainBuckets {
		fmt.Fprintf(&b, "verify_session_domains_bucket{le=\"%d\"} %d\n", bound, domCounts[i])
	}
	fmt.Fprintf(&b, "verify_session_domains_bucket{le=\"+Inf\"} %d\n", domCounts[len(domainBuckets)])
	fmt.Fprintf(&b, "verify_session_domains_sum %d\n", domSum)
	fmt.Fprintf(&b, "verify_session_domains_count %d\n", domCounts[len(domainBuckets)])

	counter(&b, "verify_refusals_of_us_total",
		"Refusals of us that count toward a stand-down, per family key (lone hosts share one label).", refusals, "mx_host")
	if standDown != nil {
		writeGauge(&b, "verify_key_stood_down", "1 while a pace key is stood down for refusals of us (plan 032).")
		sd := standDown.StoodDown()
		keys := slices.Sorted(maps.Keys(sd))
		for _, k := range keys {
			v := 0
			if sd[k] {
				v = 1
			}
			fmt.Fprintf(&b, "verify_key_stood_down{mx_host=%q} %d\n", k, v)
		}
	}
	if policyHosts != nil {
		writeGauge(&b, "ip_health_policy_hosts", "Distinct MX hosts that refused our client in the last hour.")
		fmt.Fprintf(&b, "ip_health_policy_hosts %d\n", policyHosts())
	}

	var tracked int
	if pacer != nil {
		snap := pacer.Snapshot()
		tracked = len(snap)
		sort.Slice(snap, func(i, j int) bool { return snap[i].Host < snap[j].Host })
		writeGauge(&b, "verify_rate_per_sec", "Rate the AIMD loop has settled on, per MX.")
		for _, s := range snap {
			fmt.Fprintf(&b, "verify_rate_per_sec{mx_host=%q} %g\n", s.Host, s.Rate)
		}
		writeGauge(&b, "verify_concurrency", "Concurrency the AIMD loop has settled on, per MX.")
		for _, s := range snap {
			fmt.Fprintf(&b, "verify_concurrency{mx_host=%q} %d\n", s.Host, s.Conc)
		}
		writeGauge(&b, "verify_inflight", "Session leases this process holds, per pace key (plan 028).")
		for _, s := range snap {
			fmt.Fprintf(&b, "verify_inflight{mx_host=%q} %d\n", s.Host, s.Inflight)
		}
		writeGauge(&b, "verify_mx_state", "1 for the MX's current pacer state.")
		for _, s := range snap {
			fmt.Fprintf(&b, "verify_mx_state{mx_host=%q,state=%q} 1\n", s.Host, s.State)
		}
	}

	if len(listed) > 0 {
		writeGauge(&b, "ip_health_listed", "1 if this sending IP is on the named blocklist.")
		keys := slices.Collect(maps.Keys(listed))
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		for _, k := range keys {
			v := 0
			if listed[k] {
				v = 1
			}
			fmt.Fprintf(&b, "ip_health_listed{ip=%q,list=%q} %d\n", k[0], k[1], v)
		}
	}

	// The cardinality canary. Every per-MX series above is bounded by this, so
	// if it climbs without limit the eviction has stopped working.
	writeGauge(&b, "verify_tracked_mx", "MX hosts the pacer is currently holding state for.")
	fmt.Fprintf(&b, "verify_tracked_mx %d\n", tracked)

	if complaints > 0 {
		fmt.Fprint(&b, "# HELP relay_complaints_total Spam complaints reported by the caller.\n")
		fmt.Fprint(&b, "# TYPE relay_complaints_total counter\n")
		fmt.Fprintf(&b, "relay_complaints_total %d\n", complaints)
	}

	if len(sent) > 0 || qDepth != [3]int{} {
		fmt.Fprint(&b, "# HELP relay_sent_total Outbound delivery attempts by outcome.\n")
		fmt.Fprint(&b, "# TYPE relay_sent_total counter\n")
		for _, k := range slices.Sorted(maps.Keys(sent)) {
			fmt.Fprintf(&b, "relay_sent_total{outcome=%q} %d\n", k, sent[k])
		}
		fmt.Fprint(&b, "# HELP relay_queue_depth Messages waiting, by state.\n")
		fmt.Fprint(&b, "# TYPE relay_queue_depth gauge\n")
		for i, state := range [3]string{"ready", "later", "dead"} {
			fmt.Fprintf(&b, "relay_queue_depth{state=%q} %d\n", state, qDepth[i])
		}
	}

	fmt.Fprint(&b, "# HELP verify_request_duration_seconds End-to-end POST /probe latency.\n")
	fmt.Fprint(&b, "# TYPE verify_request_duration_seconds histogram\n")
	var cumulative uint64
	for i, bound := range buckets {
		cumulative = counts[i]
		fmt.Fprintf(&b, "verify_request_duration_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(bound, 'g', -1, 64), cumulative)
	}
	fmt.Fprintf(&b, "verify_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", counts[len(buckets)])
	fmt.Fprintf(&b, "verify_request_duration_seconds_sum %g\n", sum)
	fmt.Fprintf(&b, "verify_request_duration_seconds_count %d\n", observed)

	// A service whose whole job is not leaking goroutines should say how many
	// it has. One line, no dependency.
	writeGauge(&b, "go_goroutines", "Goroutines currently running.")
	fmt.Fprintf(&b, "go_goroutines %d\n", runtime.NumGoroutine())

	return b.String()
}

func writeGauge(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
}

func counter(b *strings.Builder, name, help string, values map[string]uint64, label string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, k := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, values[k])
	}
}

func writeReplies(b *strings.Builder, replies map[[2]string]uint64) {
	const name = "verify_smtp_replies_total"
	fmt.Fprintf(b, "# HELP %s SMTP replies read, by code and classification.\n# TYPE %s counter\n", name, name)
	keys := slices.Collect(maps.Keys(replies))
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(b, "%s{code=%q,class=%q} %d\n", name, k[0], k[1], replies[k])
	}
}
