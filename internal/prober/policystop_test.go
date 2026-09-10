package prober

import "testing"

// Plan 017, option 1: the caller decides, within a bound this service keeps.
func TestPolicyStopForResolvesAndClamps(t *testing.T) {
	opts := Options{PolicyStop: 5, PolicyStopMax: 10}
	for _, c := range []struct {
		name string
		want int
		req  Request
	}{
		{"absent uses the configured default", 5, Request{}},
		{"zero means the same as absent", 5, Request{PolicyStop: 0}},
		{"a caller may lower it", 2, Request{PolicyStop: 2}},
		{"a caller may raise it to the ceiling", 10, Request{PolicyStop: 10}},
		{"over-ambitious is clamped, not refused", 10, Request{PolicyStop: 1000}},
		// A single 5.7.x can be a per-recipient policy; stopping on it would
		// throw away the batch on one server's opinion of one address, which
		// is why the config refuses 1 too.
		{"one is raised to two", 2, Request{PolicyStop: 1}},
	} {
		if got := opts.policyStopFor(c.req); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// With no maximum configured a request may lower the ceiling and never raise
// it — the safe reading of an unset bound.
func TestPolicyStopForWithoutAMaximumOnlyLowers(t *testing.T) {
	opts := Options{PolicyStop: 5}
	if got := opts.policyStopFor(Request{PolicyStop: 50}); got != 5 {
		t.Fatalf("raised past the default with no maximum set: %d", got)
	}
	if got := opts.policyStopFor(Request{PolicyStop: 3}); got != 3 {
		t.Fatalf("refused to lower: %d", got)
	}
}

// The reproduction that motivated the plan: six candidates at a Microsoft
// tenant, every one answering `550 5.4.1 Access denied`. At the default the
// sixth is never asked — one short of the ladder, every time.
func TestTheFinderLadderNeedsSixAndTheDefaultGivesFive(t *testing.T) {
	opts := Options{PolicyStop: 5, PolicyStopMax: 10}

	asked := func(stop, candidates int) int {
		n, run := 0, 0
		for i := 0; i < candidates; i++ {
			n++
			run++
			if stop > 0 && run >= stop {
				break
			}
		}
		return n
	}
	if got := asked(opts.policyStopFor(Request{}), 6); got != 5 {
		t.Fatalf("default asked %d of 6 candidates, want 5 — the bug this plan is about", got)
	}
	if got := asked(opts.policyStopFor(Request{PolicyStop: 6}), 6); got != 6 {
		t.Fatalf("with the caller's ceiling asked %d of 6, want all 6", got)
	}
}
