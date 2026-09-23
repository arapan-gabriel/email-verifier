package iphealth

import (
	"context"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Run checks once at start and then once per interval until cancelled, and a
// listing that appears between two checks is picked up by the next one — the
// behaviour main relies on when it starts `go health.Run(ctx)` (plan 027).
func TestRunChecksOnItsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var queries atomic.Int64
		var listed atomic.Bool
		lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasPrefix(host, "2.0.0.127.") { // the documented test point
				return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
			}
			if !strings.HasPrefix(host, "97.87.222.92.") {
				return nil, nil
			}
			queries.Add(1)
			if listed.Load() {
				return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
			}
			return nil, nil
		}
		store := newStore()
		h := New(Options{IP: "92.222.87.97", Zones: []string{"zen.example"}, Lookup: lookup, Interval: 15 * time.Minute, Store: store})
		// main runs the self-test first; Check refuses to act on an untested resolver.
		if _, err := h.SelfTest(t.Context()); err != nil {
			t.Fatalf("SelfTest: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { h.Run(ctx); close(done) }()

		synctest.Wait()
		if queries.Load() != 1 {
			t.Fatalf("Run made %d queries at start, want 1", queries.Load())
		}
		if burned, _ := h.Burned(); burned {
			t.Fatal("a clean check burned the node")
		}
		listed.Store(true)
		time.Sleep(14 * time.Minute)
		synctest.Wait()
		if burned, _ := h.Burned(); burned {
			t.Fatal("the listing was seen before the next scheduled check")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if burned, _ := h.Burned(); !burned {
			t.Fatal("the next scheduled check missed the listing")
		}
		if !strings.HasPrefix(store.get(Key("92.222.87.97")), "burned:") {
			t.Errorf("the verdict was not persisted: %q", store.get(Key("92.222.87.97")))
		}
		time.Sleep(45 * time.Minute)
		synctest.Wait()
		if n := queries.Load(); n != 5 {
			t.Errorf("%d checks in an hour at 15m, want 5 (t=0,15,30,45,60)", n)
		}
		cancel()
		<-done
	})
}

// With checking off, Run returns at once rather than ticking over nothing.
func TestRunDisabledReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() { New(Options{IP: "92.222.87.97"}).Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with checking off did not return")
	}
}

// A nil *Health is how a node with checking off is wired in places, and every
// method must be safe on it.
func TestNilHealthCountsNothing(t *testing.T) {
	var h *Health
	h.ObserveComplaint()
	if h.Complaints(time.Hour) != 0 || h.PolicyHosts(time.Hour) != 0 {
		t.Error("a nil Health counted something")
	}
	if paused, _ := h.SendingPaused(); paused {
		t.Error("a nil Health paused sending")
	}
}

// Policy replies age out of their window like complaints do.
func TestPolicyHostsAgeOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := New(Options{IP: "92.222.87.97"})
		h.ObservePolicy("a.test")
		time.Sleep(30 * time.Minute)
		h.ObservePolicy("b.test")
		if n := h.PolicyHosts(time.Hour); n != 2 {
			t.Fatalf("PolicyHosts = %d, want 2", n)
		}
		time.Sleep(45 * time.Minute)
		if n := h.PolicyHosts(time.Hour); n != 1 {
			t.Errorf("PolicyHosts = %d after the first aged out, want 1", n)
		}
	})
}

// The default complaint window is a day when none is configured.
func TestSendingPausedDefaultWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := New(Options{IP: "92.222.87.97", ComplaintThreshold: 1})
		h.ObserveComplaint()
		time.Sleep(23 * time.Hour)
		if paused, _ := h.SendingPaused(); !paused {
			t.Error("a complaint 23h old should still count in the default 24h window")
		}
		time.Sleep(2 * time.Hour)
		if paused, _ := h.SendingPaused(); paused {
			t.Error("a complaint 25h old still counted")
		}
	})
}

func TestRedactedZones(t *testing.T) {
	key := strings.Repeat("a", 32)
	got := RedactedZones([]string{"zen.spamhaus.org", key + ".combined.mail.abusix.zone"})
	if got[0] != "zen.spamhaus.org" || strings.Contains(got[1], key) {
		t.Errorf("RedactedZones = %v; a keyed zone's key must not survive", got)
	}
}
