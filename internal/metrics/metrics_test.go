package metrics

import (
	"strconv"
	"strings"
	"testing"
)

func lines(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if i := strings.LastIndex(l, " "); i > 0 {
			out[l[:i]] = l[i+1:]
		}
	}
	return out
}

func TestCounters(t *testing.T) {
	r := New(nil)
	r.Result("valid")
	r.Result("valid")
	r.Result("invalid")
	r.Reply(250, "valid")
	r.Reply(550, "invalid")
	r.Blocked("guarded")
	r.Pause("mx.example.test")

	got := lines(r.Render())
	for k, want := range map[string]string{
		`verify_results_total{class="valid"}`:                   "2",
		`verify_results_total{class="invalid"}`:                 "1",
		`verify_smtp_replies_total{code="250",class="valid"}`:   "1",
		`verify_smtp_replies_total{code="550",class="invalid"}`: "1",
		`verify_probe_blocked_total{reason="guarded"}`:          "1",
		`verify_pause_events_total{mx_host="mx.example.test"}`:  "1",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// A reply that was never read is not a reply. Blocked covers those.
func TestReplyIgnoresZeroCode(t *testing.T) {
	r := New(nil)
	r.Reply(0, "no_budget")
	if strings.Contains(r.Render(), "verify_smtp_replies_total{") {
		t.Error("a probe that read no reply was counted as one")
	}
}

// Prometheus histogram buckets are cumulative: a value in the 0.05 bucket is
// also in every bucket above it, and +Inf equals the total count.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New(nil)
	for _, v := range []float64{0.01, 0.2, 0.2, 3, 45} {
		r.Observe(v)
	}
	got := lines(r.Render())

	for _, tc := range []struct{ le, want string }{
		{"0.05", "1"}, // 0.01
		{"0.1", "1"},  //
		{"0.25", "3"}, // + 0.2, 0.2
		{"0.5", "3"},  //
		{"1", "3"},    //
		{"2.5", "3"},  //
		{"5", "4"},    // + 3
		{"10", "4"},   //
		{"30", "4"},   //
		{"60", "5"},   // + 45
		{"+Inf", "5"}, //
	} {
		k := `verify_request_duration_seconds_bucket{le="` + tc.le + `"}`
		if got[k] != tc.want {
			t.Errorf("%s = %q, want %q", k, got[k], tc.want)
		}
	}
	if got["verify_request_duration_seconds_count"] != "5" {
		t.Errorf("count = %q, want 5", got["verify_request_duration_seconds_count"])
	}
	sum, err := strconv.ParseFloat(got["verify_request_duration_seconds_sum"], 64)
	if err != nil || sum < 48.4 || sum > 48.5 {
		t.Errorf("sum = %q, want ~48.41", got["verify_request_duration_seconds_sum"])
	}
	// +Inf must equal the total, or the histogram is malformed.
	if got[`verify_request_duration_seconds_bucket{le="+Inf"}`] != got["verify_request_duration_seconds_count"] {
		t.Error("+Inf bucket does not equal the count")
	}
}

type fakePacer struct{ states []MXState }

func (f fakePacer) Snapshot() []MXState { return f.states }

func TestGaugesArePulledFromThePacer(t *testing.T) {
	r := New(fakePacer{states: []MXState{
		{Host: "b.test", Rate: 0.5, Conc: 1, State: "BACKOFF"},
		{Host: "a.test", Rate: 4, Conc: 2, State: "STEADY"},
	}})
	got := lines(r.Render())
	for k, want := range map[string]string{
		`verify_rate_per_sec{mx_host="a.test"}`:             "4",
		`verify_rate_per_sec{mx_host="b.test"}`:             "0.5",
		`verify_concurrency{mx_host="a.test"}`:              "2",
		`verify_mx_state{mx_host="b.test",state="BACKOFF"}`: "1",
		"verify_tracked_mx":                                 "2",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// The gauge that tells an operator the per-MX series are bounded.
func TestTrackedGaugeWithoutAPacer(t *testing.T) {
	if lines(New(nil).Render())["verify_tracked_mx"] != "0" {
		t.Error("verify_tracked_mx missing or wrong with no pacer attached")
	}
}

func TestRenderIsWellFormed(t *testing.T) {
	r := New(fakePacer{states: []MXState{{Host: "a.test", Rate: 1, Conc: 1, State: "STEADY"}}})
	r.Result("valid")
	r.Observe(0.3)
	out := r.Render()

	for _, name := range []string{
		"verify_results_total", "verify_smtp_replies_total", "verify_probe_blocked_total",
		"verify_pause_events_total", "verify_rate_per_sec", "verify_concurrency",
		"verify_mx_state", "verify_tracked_mx", "verify_request_duration_seconds", "go_goroutines",
	} {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE line", name)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Count(l, " ") == 0 {
			t.Errorf("sample line has no value: %q", l)
		}
	}
}
