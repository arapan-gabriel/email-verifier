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

// MXState is one row of what the pacer is currently tracking.
type MXState struct {
	Host string
	Rate float64
	// MaxRate is the band's ceiling. The gauges do not report it, but the
	// operator's band view needs it and this is already the snapshot.
	MaxRate float64
	Conc    int
	State   string
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

	results  map[string]uint64 // class
	replies  map[[2]string]uint64
	blocked  map[string]uint64  // reason
	pauses   map[string]uint64  // mx host
	listed   map[[2]string]bool // {ip, list} -> listed
	counts   []uint64           // duration histogram, one per bucket plus +Inf
	sum      float64
	observed uint64

	pacer Pacer
}

// New returns a Registry. pacer may be nil, in which case the per-MX gauges are
// simply absent rather than wrong.
func New(pacer Pacer) *Registry {
	return &Registry{
		results: map[string]uint64{},
		replies: map[[2]string]uint64{},
		blocked: map[string]uint64{},
		pauses:  map[string]uint64{},
		listed:  map[[2]string]bool{},
		counts:  make([]uint64, len(buckets)+1),
		pacer:   pacer,
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
	pauses := maps.Clone(r.pauses)
	listed := maps.Clone(r.listed)
	counts := slices.Clone(r.counts)
	sum, observed := r.sum, r.observed
	pacer := r.pacer
	r.mu.Unlock()

	var b strings.Builder

	counter(&b, "verify_results_total", "Addresses answered, by classification.", results, "class")
	writeReplies(&b, replies)
	counter(&b, "verify_probe_blocked_total",
		"Probes this service declined to send, by reason.", blocked, "reason")
	counter(&b, "verify_pause_events_total",
		"Times the pacer stood an MX down after throttling at the floor of its band.", pauses, "mx_host")

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
