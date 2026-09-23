package metrics

import (
	"strings"
	"sync"
	"testing"
)

// The relay and IP-health recorders, each asserted on what a scrape then says
// (plan 027: one behavioural test per recorder).
func TestRelayAndHealthRecorders(t *testing.T) {
	r := New(nil)
	if out := r.Render(); strings.Contains(out, "relay_") || strings.Contains(out, "ip_health_listed") {
		t.Fatalf("a fresh registry rendered relay or health series:\n%s", out)
	}
	r.Sent("delivered")
	r.Sent("delivered")
	r.Sent("deferred")
	r.QueueDepth(3, 2, 1)
	r.Complaint()
	r.IPListed("192.0.2.1", "zen.example", true)
	r.IPListed("192.0.2.1", "bl.example", false)
	r.IPListed("192.0.2.0", "zen.example", false)
	got := lines(r.Render())
	for k, want := range map[string]string{
		`relay_sent_total{outcome="delivered"}`:               "2",
		`relay_sent_total{outcome="deferred"}`:                "1",
		`relay_queue_depth{state="ready"}`:                    "3",
		`relay_queue_depth{state="later"}`:                    "2",
		`relay_queue_depth{state="dead"}`:                     "1",
		`relay_complaints_total`:                              "1",
		`ip_health_listed{ip="192.0.2.1",list="zen.example"}`: "1",
		`ip_health_listed{ip="192.0.2.1",list="bl.example"}`:  "0",
		`ip_health_listed{ip="192.0.2.0",list="zen.example"}`: "0",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %s", k, got[k], want)
		}
	}
	// A queue that drained to zero still reports its depth once a send happened.
	r.QueueDepth(0, 0, 0)
	if lines(r.Render())[`relay_queue_depth{state="ready"}`] != "0" {
		t.Error("an empty queue stopped reporting its depth")
	}
}

// SetPacer wires the gauge source after construction, as main does.
func TestSetPacerAfterConstruction(t *testing.T) {
	r := New(nil)
	r.SetPacer(fakePacer{states: []MXState{{Host: "mx.test", Rate: 2, Conc: 1, State: "STEADY"}}})
	if lines(r.Render())[`verify_rate_per_sec{mx_host="mx.test"}`] != "2" {
		t.Error("a pacer set after New is not scraped")
	}
}

// Every recorder racing a scrape (plan 027, Design 8). Render used to read the
// relay map and counters after releasing the lock, and a concurrent map read
// and write is a fatal error, not a stale number.
func TestRecordersRaceARender(t *testing.T) {
	r := New(fakePacer{states: []MXState{{Host: "mx.test", Rate: 1, Conc: 1, State: "STEADY"}}})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				r.Result("valid")
				r.Reply(250, "valid")
				r.Blocked("paused")
				r.Pause("mx.test")
				r.IPListed("192.0.2.1", "zen.example", i%2 == 0)
				r.Sent([]string{"delivered", "deferred", "no_mx"}[(g+i)%3])
				r.Complaint()
				r.QueueDepth(i, g, 0)
				r.Observe(0.2)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = r.Render()
			}
		}()
	}
	wg.Wait()
	if got := lines(r.Render())["relay_complaints_total"]; got != "1600" {
		t.Errorf("complaints = %s, want 1600", got)
	}
}
