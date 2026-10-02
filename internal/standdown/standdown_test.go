package standdown

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// zstore is an in-memory Redis subset: sorted sets and DEL, enough for the guard.
type zstore struct {
	mu   sync.Mutex
	sets map[string]map[string]int64
	err  error
}

func newZ() *zstore { return &zstore{sets: map[string]map[string]int64{}} }

func (z *zstore) Do(_ context.Context, args ...string) (any, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.err != nil {
		return nil, z.err
	}
	switch strings.ToUpper(args[0]) {
	case "ZADD":
		score, _ := strconv.ParseInt(args[2], 10, 64)
		if z.sets[args[1]] == nil {
			z.sets[args[1]] = map[string]int64{}
		}
		z.sets[args[1]][args[3]] = score
		return int64(1), nil
	case "ZREMRANGEBYSCORE":
		upper, _ := strconv.ParseInt(strings.TrimPrefix(args[3], "("), 10, 64)
		var n int64
		for m, sc := range z.sets[args[1]] {
			if sc < upper {
				delete(z.sets[args[1]], m)
				n++
			}
		}
		return n, nil
	case "ZCARD":
		return int64(len(z.sets[args[1]])), nil
	case "PEXPIRE":
		return int64(1), nil
	case "DEL":
		delete(z.sets, args[1])
		return int64(1), nil
	}
	return nil, errors.New("unsupported " + args[0])
}

type pauseCall struct {
	key   string
	until time.Time
}

type fakePauser struct {
	mu      sync.Mutex
	paused  []pauseCall
	resumed []string
}

func (f *fakePauser) PauseKey(_ context.Context, key string, until time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, pauseCall{key, until})
	return nil
}

func (f *fakePauser) ResumeKey(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, key)
	return nil
}

type counting struct{ keys []string }

func (c *counting) RefusalOfUs(key string) { c.keys = append(c.keys, key) }

const (
	unexplained = "550 5.7.1 Client host rejected: Error Protokoll"
	blocklisted = "550 5.7.1 Service unavailable; Client host [92.222.87.97] blocked using Spamhaus"
	dbeb        = "550 5.4.1 Recipient address rejected: Access denied. AS(201806281)"
)

func guard(t *testing.T, z *zstore, p *fakePauser, rec *counting, now *time.Time) *Guard {
	t.Helper()
	return New(z, p, rec, Options{Hosts: 3, Window: 30 * time.Minute, Pause: 6 * time.Hour,
		Now: func() time.Time { return *now }})
}

func tenant(n int) string { return "tenant" + strconv.Itoa(n) + "-de.mail.protection.outlook.com" }

// Rule A: one refusal naming a blocklist stands the whole family down at once,
// and only that family.
func TestABlocklistRefusalStandsTheFamilyDown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	g := guard(t, newZ(), p, &counting{}, &now)

	if rule := g.Observe(t.Context(), tenant(1), blocklisted); rule != "A" {
		t.Fatalf("rule = %q, want A", rule)
	}
	if len(p.paused) != 1 || p.paused[0].key != "@microsoft-eop" || !p.paused[0].until.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("paused %+v, want @microsoft-eop for 6h", p.paused)
	}
	if !g.StoodDown()["@microsoft-eop"] || g.StoodDown()["@google"] {
		t.Errorf("stood down = %v, want only @microsoft-eop", g.StoodDown())
	}
	// Further refusals while down change nothing.
	if rule := g.Observe(t.Context(), tenant(2), blocklisted); rule != "" || len(p.paused) != 1 {
		t.Errorf("a second refusal re-paused: rule %q, %d pauses", rule, len(p.paused))
	}
}

// Rule B: N distinct hosts of one family within the window; N-1 is not enough.
func TestThreeDistinctTenantsStandEOPDown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	g := guard(t, newZ(), p, &counting{}, &now)

	for i := 1; i <= 2; i++ {
		if rule := g.Observe(t.Context(), tenant(i), unexplained); rule != "" {
			t.Fatalf("host %d stood the family down early (rule %q)", i, rule)
		}
		now = now.Add(time.Minute)
	}
	if rule := g.Observe(t.Context(), tenant(3), unexplained); rule != "B" {
		t.Fatalf("third distinct host: rule %q, want B", rule)
	}
	if len(p.paused) != 1 || p.paused[0].key != "@microsoft-eop" {
		t.Errorf("paused %+v", p.paused)
	}
}

// Outside the window the earlier hosts no longer count.
func TestHostsOutsideTheWindowDoNotCount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	g := guard(t, newZ(), p, &counting{}, &now)
	g.Observe(t.Context(), tenant(1), unexplained)
	g.Observe(t.Context(), tenant(2), unexplained)
	now = now.Add(31 * time.Minute)
	if rule := g.Observe(t.Context(), tenant(3), unexplained); rule != "" {
		t.Errorf("stale hosts counted: rule %q", rule)
	}
}

// Microsoft's 5.4.1 is a tenant's setting, not our reputation: never counted.
func TestDirectoryBlockingIsNeverCounted(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	rec := &counting{}
	g := guard(t, newZ(), p, rec, &now)
	for i := 1; i <= 5; i++ {
		g.Observe(t.Context(), tenant(i), dbeb)
	}
	for _, r := range []string{"550 5.7.1 Must issue a STARTTLS command first", "550 5.7.1 Relay access denied", "552 5.2.2 Mailbox full"} {
		g.Observe(t.Context(), "mx.example.de", r)
	}
	if len(p.paused) != 0 || len(rec.keys) != 0 {
		t.Errorf("benign replies paused %v / counted %v", p.paused, rec.keys)
	}
}

// A lone host is its own key: a blocklist reply pauses that host only, and the
// same host refusing over and over is one host, never N.
func TestALoneHostCanOnlyPauseItself(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	g := guard(t, newZ(), p, &counting{}, &now)
	for range 5 {
		g.Observe(t.Context(), "mx.lonely.de", unexplained)
	}
	if len(p.paused) != 0 {
		t.Fatalf("one host refusing repeatedly paused %v", p.paused)
	}
	if rule := g.Observe(t.Context(), "mx.lonely.de", blocklisted); rule != "A" {
		t.Fatalf("rule %q, want A", rule)
	}
	if p.paused[0].key != "mx.lonely.de" {
		t.Errorf("paused %q, want only the host", p.paused[0].key)
	}
}

// T-Online's standing dial-up refusal is counted (it is about us) but names no
// list and is one host, so it can never stand anything down.
func TestTOnlinesStandingRefusalNeverStandsDown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakePauser{}
	g := guard(t, newZ(), p, &counting{}, &now)
	for range 10 {
		g.Observe(t.Context(), "smtp-01.tld.t-online.de", "554 IP=92.222.87.97 - Dialup/transient IP not allowed")
	}
	if len(p.paused) != 0 {
		t.Errorf("T-Online paused %v", p.paused)
	}
}

// No Redis, no evidence, no stand-down — and no panic.
func TestRedisDownInventsNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	z := newZ()
	z.err = errors.New("connection refused")
	p := &fakePauser{}
	g := guard(t, z, p, &counting{}, &now)
	if rule := g.Observe(t.Context(), tenant(1), blocklisted); rule != "" || len(p.paused) != 0 {
		t.Errorf("stood down without a store: rule %q, %v", rule, p.paused)
	}
}

func TestResumeLiftsAndForgets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	z := newZ()
	p := &fakePauser{}
	g := guard(t, z, p, &counting{}, &now)
	g.Observe(t.Context(), tenant(1), blocklisted)
	if err := g.Resume(t.Context(), tenant(9)); err != nil {
		t.Fatal(err)
	}
	if len(p.resumed) != 1 || p.resumed[0] != "@microsoft-eop" {
		t.Errorf("resumed %v", p.resumed)
	}
	if _, ok := z.sets[RefusalsKey("@microsoft-eop")]; ok {
		t.Error("refusers kept after resume")
	}
	if _, ok := g.StoodDown()["@microsoft-eop"]; ok {
		t.Error("gauge still lists the key after resume")
	}
}

// The gauge clears on its own when the pause runs out.
func TestStoodDownClearsAtExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := guard(t, newZ(), &fakePauser{}, &counting{}, &now)
	g.Observe(t.Context(), tenant(1), blocklisted)
	now = now.Add(7 * time.Hour)
	if g.StoodDown()["@microsoft-eop"] {
		t.Error("still stood down after the pause")
	}
}

func TestNilGuardIsInert(t *testing.T) {
	var g *Guard
	if g.Observe(context.Background(), "mx", blocklisted) != "" || g.Resume(context.Background(), "mx") != nil || len(g.StoodDown()) != 0 {
		t.Error("nil guard acted")
	}
}

func TestClassifierLists(t *testing.T) {
	for _, r := range []string{dbeb, "550 Must use TLS", "554 5.7.64 TenantAttribution; Relay Access Denied"} {
		if !Benign(r) {
			t.Errorf("not benign: %q", r)
		}
	}
	for _, r := range []string{blocklisted, "550 listed as abusive by MagicSpam", "550 5.7.1 ... on our block list (S3150)"} {
		if !NamesBlocklist(r) {
			t.Errorf("not rule A: %q", r)
		}
	}
	for _, r := range []string{"450 4.2.0 Greylisted, see postgrey", unexplained} {
		if NamesBlocklist(r) {
			t.Errorf("rule A on %q", r)
		}
	}
}
