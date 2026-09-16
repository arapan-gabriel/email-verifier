package iphealth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu sync.Mutex
	kv map[string]string
}

func newStore() *fakeStore { return &fakeStore{kv: map[string]string{}} }
func (f *fakeStore) Get(_ context.Context, k string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[k]
	return v, ok, nil
}
func (f *fakeStore) Set(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[k] = v
	return nil
}
func (f *fakeStore) get(k string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kv[k]
}

// answers maps a full query name to the addresses a resolver returns.
func answers(m map[string][]string) LookupFunc {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		v, ok := m[host]
		if !ok {
			return nil, nil // NXDOMAIN: not listed
		}
		out := make([]netip.Addr, 0, len(v))
		for _, a := range v {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

// A resolver that answers everything is the failure this package exists to
// survive: it reads as "listed on every zone", and pausing on that is an outage
// caused by a resolver misconfiguration.
func stubResolverAnsweringEverything() LookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.255.255.254")}, nil
	}
}

func healthy(lookup LookupFunc, store Store) *Health {
	return New(Options{IP: "92.222.87.97", Zones: []string{"zen.spamhaus.org"}, Lookup: lookup, Store: store})
}

func TestSelfTestAcceptsAWorkingResolver(t *testing.T) {
	h := healthy(answers(map[string][]string{
		"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"}, // the documented test point
	}), newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
}

// Two shapes of broken resolver, both of which would otherwise read as "listed
// on every zone" and stand the node down for a resolver misconfiguration.
func TestSelfTestRejectsBrokenResolvers(t *testing.T) {
	t.Run("refuses every query", func(t *testing.T) {
		// 127.255.255.254 is the "your query was refused" sentinel. The test
		// point therefore comes back unlisted, which is itself the giveaway.
		_, err := healthy(stubResolverAnsweringEverything(), newStore()).SelfTest(t.Context())
		if err == nil {
			t.Fatal("a resolver refusing every query passed the self-test")
		}
		if !strings.Contains(err.Error(), "test point") {
			t.Errorf("error = %v, want it to name the test point", err)
		}
	})

	t.Run("lists everything", func(t *testing.T) {
		// A wildcard that answers 127.0.0.2 to anything passes the test point
		// and is caught by the clean point.
		lookup := func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}
		_, err := healthy(lookup, newStore()).SelfTest(t.Context())
		if err == nil {
			t.Fatal("a resolver listing every query passed the self-test")
		}
		if !strings.Contains(err.Error(), "clean point") {
			t.Errorf("error = %v, want it to name the clean point", err)
		}
	})
}

func TestSelfTestRejectsAResolverThatCannotQueryTheZone(t *testing.T) {
	// Everything NXDOMAINs, including the test point: the zone is unreachable.
	h := healthy(answers(nil), newStore())
	if _, err := h.SelfTest(t.Context()); err == nil {
		t.Fatal("a resolver that cannot query the zone passed the self-test")
	}
}

// The whole point: an untrusted resolver disables checking rather than
// triggering it.
func TestCheckRefusesWithoutASelfTest(t *testing.T) {
	h := healthy(answers(map[string][]string{
		"97.87.222.92.zen.spamhaus.org": {"127.0.0.2"},
	}), newStore())
	if _, err := h.Check(t.Context()); err == nil {
		t.Fatal("Check ran without a passing self-test")
	}
	if burned, _ := h.Burned(); burned {
		t.Error("the node was stood down on the word of an untrusted resolver")
	}
}

func TestListingBurnsTheNode(t *testing.T) {
	store := newStore()
	h := healthy(answers(map[string][]string{
		"2.0.0.127.zen.spamhaus.org":    {"127.0.0.2"},
		"97.87.222.92.zen.spamhaus.org": {"127.0.0.4"},
	}), store)
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	rep, err := h.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rep.Burned {
		t.Fatal("a confirmed listing did not burn the node")
	}
	burned, reason := h.Burned()
	if !burned || !strings.Contains(reason, "zen.spamhaus.org") {
		t.Errorf("Burned = %v, %q", burned, reason)
	}
	if got := store.get(Key("92.222.87.97")); !strings.HasPrefix(got, "burned:") {
		t.Errorf("persisted %q, want a burned marker", got)
	}
}

func TestCleanIPIsNotBurned(t *testing.T) {
	h := healthy(answers(map[string][]string{
		"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"},
	}), newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	rep, _ := h.Check(t.Context())
	if rep.Burned {
		t.Errorf("a clean IP was burned: %s", rep.Reason)
	}
}

// A query that fails is not a listing. Treating it as one is the false positive
// this package exists to avoid.
func TestQueryFailureIsNotAListing(t *testing.T) {
	calls := 0
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		calls++
		if strings.HasPrefix(host, "2.0.0.127") {
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}
		if strings.HasPrefix(host, "1.0.0.127") {
			return nil, nil
		}
		return nil, errors.New("SERVFAIL")
	}
	h := healthy(lookup, newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	rep, err := h.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Burned {
		t.Error("a failed query burned the node")
	}
}

// Pausing on this would hand any MX that starts answering 5.7.x a way to stand
// the node down.
func TestPolicyObservationsNeverBurn(t *testing.T) {
	h := healthy(answers(map[string][]string{
		"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"},
	}), newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"a.test", "b.test", "c.test", "d.test", "e.test"} {
		h.ObservePolicy(host)
	}
	if n := h.PolicyHosts(time.Hour); n != 5 {
		t.Errorf("PolicyHosts = %d, want 5", n)
	}
	if burned, _ := h.Burned(); burned {
		t.Error("a policy rate stood the node down; it is an inference, not a fact about the IP")
	}
	// Repeats from one host are one host.
	h2 := healthy(answers(nil), newStore())
	for range 20 {
		h2.ObservePolicy("noisy.test")
	}
	if n := h2.PolicyHosts(time.Hour); n != 1 {
		t.Errorf("PolicyHosts = %d for one repeated host, want 1", n)
	}
}

func TestResumeClearsThePause(t *testing.T) {
	store := newStore()
	h := healthy(answers(map[string][]string{
		"2.0.0.127.zen.spamhaus.org":    {"127.0.0.2"},
		"97.87.222.92.zen.spamhaus.org": {"127.0.0.4"},
	}), store)
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Resume(t.Context())
	if burned, _ := h.Burned(); burned {
		t.Error("Resume did not clear the pause")
	}
}

// Without a resolver there is no checking, and no fallback to the host's — on
// the deployed node that is a stub.
func TestDisabledWithoutAResolver(t *testing.T) {
	h := New(Options{IP: "92.222.87.97"})
	if h.Enabled() {
		t.Error("checking reported enabled with no resolver")
	}
	if _, err := h.Check(t.Context()); err == nil {
		t.Error("Check ran with checking disabled")
	}
	if burned, _ := h.Burned(); burned {
		t.Error("a disabled checker burned the node")
	}
}

// UCEPROTECT L3 lists a whole ASN: measured on our own IP while Spamhaus was
// clean and both Gmail and Microsoft accepted the session.
func TestDefaultZonesExcludeASNWideLists(t *testing.T) {
	for _, z := range DefaultZones {
		if strings.Contains(z, "uceprotect") {
			t.Errorf("%s is on by default; it lists an ASN, not an address", z)
		}
	}
	if len(DefaultZones) == 0 {
		t.Error("no default zones")
	}
}

func TestReverse(t *testing.T) {
	if got := reverse("92.222.87.97"); got != "97.87.222.92" {
		t.Errorf("reverse = %q", got)
	}
}

func TestNilHealthIsSafe(t *testing.T) {
	var h *Health
	if burned, _ := h.Burned(); burned {
		t.Error("nil Health reported burned")
	}
	h.ObservePolicy("mx.test")
	h.Resume(context.Background())
	if h.Enabled() {
		t.Error("nil Health reported enabled")
	}
}

// NXDOMAIN is how a DNSBL says "not listed", and it is the overwhelmingly
// common answer. Go reports it as an error; treating that as an unreachable
// zone makes every clean zone look broken, so the self-test fails permanently
// and checking never runs. Found on a live resolver, not by the fakes above —
// which is why this one returns the error a real resolver returns.
func TestNXDOMAINIsNotListedRatherThanAFailure(t *testing.T) {
	nx := &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasPrefix(host, "2.0.0.127") { // the listed test point
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}
		return nil, nx // everything else: not listed
	}
	h := healthy(lookup, newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatalf("SelfTest rejected a working resolver because clean answers are NXDOMAIN: %v", err)
	}
	rep, err := h.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Burned {
		t.Error("a clean address was burned")
	}
	if rep.Listed["zen.spamhaus.org"] {
		t.Error("NXDOMAIN was read as a listing")
	}
}

// A genuine failure still is one — it must not be quietly read as "clean".
func TestServfailIsStillAFailure(t *testing.T) {
	boom := &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasPrefix(host, "2.0.0.127") {
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}
		if strings.HasPrefix(host, "1.0.0.127") {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		}
		return nil, boom
	}
	h := healthy(lookup, newStore())
	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	rep, err := h.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Burned {
		t.Error("a SERVFAIL burned the node")
	}
	if _, ok := rep.Listed["zen.spamhaus.org"]; ok {
		t.Error("a failed query was recorded as a verdict")
	}
}

// The reason plan 021 exists: coverage must be addable without risking the
// coverage already there. A third zone that cannot answer used to disable the
// two that could, so the only safe number of zones was the number already
// configured.
func TestSelfTestDropsOneZoneAndKeepsTheRest(t *testing.T) {
	const abusix = "KEY.combined.mail.abusix.zone"
	h := New(Options{
		IP:    "92.222.87.97",
		Zones: []string{"zen.spamhaus.org", "bl.spamcop.net", abusix},
		// Spamhaus and SpamCop answer their test points; Abusix answers
		// nothing, which is what a wrong or expired subscription key looks
		// like from here.
		Lookup: answers(map[string][]string{
			"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"},
			"2.0.0.127.bl.spamcop.net":   {"127.0.0.2"},
		}),
		Store: newStore(),
	})

	result, err := h.SelfTest(t.Context())
	if err != nil {
		t.Fatalf("one unanswerable zone disabled the whole check: %v", err)
	}
	if len(result.Dropped) != 1 || result.Dropped[0].Zone != abusix {
		t.Fatalf("dropped = %+v, want exactly %s", result.Dropped, abusix)
	}
	if !strings.Contains(result.Dropped[0].Reason.Error(), "test point") {
		t.Errorf("reason = %v, want it to name the test point", result.Dropped[0].Reason)
	}

	// The survivors are what gets queried from here on, so a zone absent from
	// Zones() — and therefore from ip_health_listed — means "not covered"
	// rather than "covered and clean".
	got := h.Zones()
	want := []string{"zen.spamhaus.org", "bl.spamcop.net"}
	if len(got) != len(want) {
		t.Fatalf("Zones() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Zones() = %v, want %v", got, want)
		}
	}
}

// The old all-or-nothing behaviour is still correct for the case it was written
// for: a resolver that cannot do DNSBL at all.
func TestSelfTestStillDisablesWhenNoZonePasses(t *testing.T) {
	h := New(Options{
		IP:     "92.222.87.97",
		Zones:  []string{"zen.spamhaus.org", "bl.spamcop.net"},
		Lookup: answers(map[string][]string{}),
		Store:  newStore(),
	})

	result, err := h.SelfTest(t.Context())
	if err == nil {
		t.Fatal("a resolver that answers no zone passed the self-test")
	}
	if len(result.Dropped) != 2 {
		t.Errorf("dropped = %+v, want both zones reported", result.Dropped)
	}
	// Check must still refuse: not trusted means not acted upon.
	if _, err := h.Check(t.Context()); err == nil {
		t.Error("Check ran against a resolver that failed every zone")
	}
}

// A stub answering everything is rejected per zone, not only in aggregate —
// otherwise one wildcard zone would survive alongside real ones and pause the
// node on its say-so.
func TestSelfTestDropsAWildcardZoneIndividually(t *testing.T) {
	const wildcard = "wildcard.example"
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasSuffix(host, wildcard) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil // lists anything
		}
		if host == "2.0.0.127.zen.spamhaus.org" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}
		return nil, nil
	}
	h := New(Options{
		IP:     "92.222.87.97",
		Zones:  []string{"zen.spamhaus.org", wildcard},
		Lookup: lookup,
		Store:  newStore(),
	})

	result, err := h.SelfTest(t.Context())
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(result.Dropped) != 1 || result.Dropped[0].Zone != wildcard {
		t.Fatalf("dropped = %+v, want exactly %s", result.Dropped, wildcard)
	}
	if !strings.Contains(result.Dropped[0].Reason.Error(), "clean point") {
		t.Errorf("reason = %v, want it to name the clean point", result.Dropped[0].Reason)
	}
	if zones := h.Zones(); len(zones) != 1 || zones[0] != "zen.spamhaus.org" {
		t.Fatalf("Zones() = %v, want only zen.spamhaus.org", zones)
	}
}

// Plan 023, and the reason it exists. rbl.your-server.de is a real list —
// Hetzner's own, the one that refused this IP on ladder day 5 — and on
// 2026-09-16 it answered 127.0.0.2 for RFC 5782's clean point as well as for
// its test point, because its entries come from what receiving servers report
// and somebody's misconfigured host reported its own loopback. Two unrelated
// addresses answered nothing, so it is not a resolver answering everything, and
// the old test dropped it anyway.
func TestAListCarryingTheCleanPointIsKeptWithACaveat(t *testing.T) {
	const hetzner = "rbl.your-server.de"
	h := New(Options{
		IP:    "92.222.87.97",
		Zones: []string{"zen.spamhaus.org", hetzner},
		Lookup: answers(map[string][]string{
			"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"},
			"2.0.0.127." + hetzner:       {"127.0.0.2"}, // "Local RBL", a static test point
			"1.0.0.127." + hetzner:       {"127.0.0.2"}, // "Last seen 2026-09-16 08:30:03"
		}),
		Store: newStore(),
	})

	result, err := h.SelfTest(t.Context())
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Fatalf("dropped = %+v, want nothing dropped", result.Dropped)
	}
	if len(result.Caveats) != 1 || result.Caveats[0].Zone != hetzner {
		t.Fatalf("caveats = %+v, want exactly %s", result.Caveats, hetzner)
	}
	// The caveat has to say what is wrong and why the zone survived it, because
	// a journal line is where this gets read a year from now.
	detail := result.Caveats[0].Reason.Error()
	for _, want := range []string{hetzner, "127.0.0.1", "192.0.2.1"} {
		if !strings.Contains(detail, want) {
			t.Errorf("caveat = %q, want it to name %s", detail, want)
		}
	}
	// And the zone is watched: a caveat is not a soft drop.
	if zones := h.Zones(); len(zones) != 2 {
		t.Fatalf("Zones() = %v, want both zones watched", zones)
	}
}

// The second opinion costs two queries per zone, so it is asked only when the
// clean point actually comes back listed — every zone in normal service still
// self-tests in exactly two lookups.
func TestTheSecondOpinionIsOnlyAskedWhenTheCleanPointIsListed(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	base := answers(map[string][]string{"2.0.0.127.zen.spamhaus.org": {"127.0.0.2"}})
	h := healthy(func(ctx context.Context, host string) ([]netip.Addr, error) {
		mu.Lock()
		asked = append(asked, host)
		mu.Unlock()
		return base(ctx, host)
	}, newStore())

	if _, err := h.SelfTest(t.Context()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(asked) != 2 {
		t.Fatalf("queries = %v, want the test point and the clean point alone", asked)
	}
	for _, host := range asked {
		for _, point := range unlistablePoints {
			if strings.HasPrefix(host, point.prefix+".") {
				t.Errorf("asked %q for a zone whose clean point was clean", host)
			}
		}
	}
}

// One bogus entry is the whole finding, so a single documentation address in
// the list must not decide the zone — that would reproduce the defect one
// address further along. A stub has to answer both.
func TestOneJunkEntryInTheDocumentationRangeDoesNotCostTheZone(t *testing.T) {
	const zone = "junk.example"
	h := healthy(answers(map[string][]string{
		"2.0.0.127." + zone: {"127.0.0.2"},
		"1.0.0.127." + zone: {"127.0.0.2"},
		"1.2.0.192." + zone: {"127.0.0.2"}, // 192.0.2.1, listed too
		"9.9.9.9." + zone:   {"127.0.0.2"},
	}), newStore())
	h.zones = []string{zone}

	result, err := h.SelfTest(t.Context())
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Fatalf("dropped = %+v, want the zone kept on the second point", result.Dropped)
	}
	if len(result.Caveats) != 1 {
		t.Fatalf("caveats = %+v, want one", result.Caveats)
	}
	if detail := result.Caveats[0].Reason.Error(); !strings.Contains(detail, "203.0.113.1") {
		t.Errorf("caveat = %q, want it to name the point that answered clean", detail)
	}
}

// An opinion that cannot be taken is not an opinion. A zone whose answers we
// cannot read is what the self-test refuses, and a listed clean point with no
// second reading is exactly that.
func TestASecondOpinionThatCannotBeTakenCostsTheZone(t *testing.T) {
	const zone = "unreachable.example"
	listed := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	h := healthy(func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasPrefix(host, "2.0.0.127.") || strings.HasPrefix(host, "1.0.0.127.") {
			return listed, nil
		}
		return nil, &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
	}, newStore())
	h.zones = []string{zone}

	result, err := h.SelfTest(t.Context())
	if err == nil {
		t.Fatal("a zone whose second opinion errored was trusted")
	}
	if len(result.Dropped) != 1 {
		t.Fatalf("dropped = %+v, want the zone", result.Dropped)
	}
	if reason := result.Dropped[0].Reason.Error(); !strings.Contains(reason, "unreachable") {
		t.Errorf("reason = %q, want it to say the second opinion could not be taken", reason)
	}
}
