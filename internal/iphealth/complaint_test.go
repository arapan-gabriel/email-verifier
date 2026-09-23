package iphealth

import (
	"testing"
	"testing/synctest"
	"time"
)

// Complaints and blocklist entries are different switches on purpose. A listing
// is about the address and stops both legs; a complaint spike is about messages
// and stops only sending — pausing the probe would not improve anything while
// costing every customer their answers.
func TestAComplaintSpikePausesSendingAndNothingElse(t *testing.T) {
	h := New(Options{IP: "203.0.113.1", ComplaintWindow: time.Hour, ComplaintThreshold: 3})

	for range 2 {
		h.ObserveComplaint()
	}
	if paused, _ := h.SendingPaused(); paused {
		t.Fatal("paused below the threshold")
	}
	h.ObserveComplaint()
	paused, reason := h.SendingPaused()
	if !paused {
		t.Fatal("the threshold was reached and sending did not pause")
	}
	if reason == "" {
		t.Error("a pause with no reason is an outage nobody can explain")
	}
	// The blocklist question is untouched: nothing here says the IP is listed.
	if burned, _ := h.Burned(); burned {
		t.Error("a complaint spike must not read as a blocklist entry")
	}
}

// A complaint from last month says nothing about today.
// On the synctest clock (plan 027), so the window is the real one rather than
// a millisecond chosen to keep a wall-clock sleep short.
func TestComplaintsAgeOutOfTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := New(Options{IP: "203.0.113.1", ComplaintWindow: time.Hour, ComplaintThreshold: 1})
		h.ObserveComplaint()
		time.Sleep(59 * time.Minute)
		if paused, _ := h.SendingPaused(); !paused {
			t.Fatal("a complaint inside the window stopped counting")
		}
		time.Sleep(2 * time.Minute)
		if paused, _ := h.SendingPaused(); paused {
			t.Fatal("an expired complaint still counted")
		}
	})
}

// Zero is off, which is what a node with no relay needs: a threshold nobody
// chose should not stop mail on its own.
func TestAZeroThresholdNeverPauses(t *testing.T) {
	h := New(Options{IP: "203.0.113.1", ComplaintWindow: time.Hour})
	for range 100 {
		h.ObserveComplaint()
	}
	if paused, _ := h.SendingPaused(); paused {
		t.Fatal("paused with no threshold configured")
	}
}
