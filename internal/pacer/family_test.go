package pacer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
)

func TestPaceKey(t *testing.T) {
	cases := []struct{ host, want string }{
		// Microsoft 365: every tenant is one system.
		{"contoso-com.mail.protection.outlook.com", "@microsoft-eop"},
		{"rotweisskemberg-de0i1c.mail.protection.outlook.com", "@microsoft-eop"},
		{"Contoso-COM.Mail.Protection.Outlook.com.", "@microsoft-eop"},
		{"  contoso-com.mail.protection.outlook.com  ", "@microsoft-eop"},
		// Google: the primary, its alternates, the legacy names and the new MX.
		{"aspmx.l.google.com", "@google"},
		{"alt3.aspmx.l.google.com", "@google"},
		{"aspmx2.googlemail.com", "@google"},
		{"smtp.google.com", "@google"},
		{"mxa-00123.gslb.pphosted.com", "@proofpoint"},
		{"eu-smtp-inbound-1.mimecast.com", "@mimecast"},
		{"mx01.hornetsecurity.com", "@hornetsecurity"},
		{"mx00.ionos.de", "@ionos"},
		{"mx01.kundenserver.de", "@ionos"},
		// A suffix matches on a label boundary only.
		{"evil-outlook.com", "evil-outlook.com"},
		{"notmail.protection.outlook.com", "notmail.protection.outlook.com"},
		{"mail.protection.outlook.com.attacker.net", "mail.protection.outlook.com.attacker.net"},
		{"outlook.com.attacker.net", "outlook.com.attacker.net"},
		{"fakeionos.de", "fakeionos.de"},
		// A customer's own name that merely contains a provider's.
		{"mx01.ionos.de.wheels-performance.de", "mx01.ionos.de.wheels-performance.de"},
		// Consumer Outlook is not EOP's tenant farm, and keeps today's key.
		{"outlook-com.olc.protection.outlook.com", "outlook-com.olc.protection.outlook.com"},
		// Anything unmatched is returned exactly as given — today's behaviour.
		{"Mail.Example.DE.", "Mail.Example.DE."},
		{"", ""},
		// A key is already a key.
		{"@google", "@google"},
	}
	for _, c := range cases {
		if got := PaceKey(c.host); got != c.want {
			t.Errorf("PaceKey(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestMalformedFamiliesTableIsABuildDefect(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      `{`,
		"key without @": `{"families":[{"suffix":"x.com","key":"x"}]}`,
		"bare @":        `{"families":[{"suffix":"x.com","key":"@"}]}`,
		"empty suffix":  `{"families":[{"suffix":" . ","key":"@x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("mustFamilies accepted a malformed table")
				}
			}()
			mustFamilies([]byte(raw))
		})
	}
}

// keyTaker records the key every token was drawn under.
type keyTaker struct {
	mu   sync.Mutex
	keys []string
	err  error
}

func (k *keyTaker) Take(_ context.Context, key string, _, _ float64) (limiter.Decision, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return limiter.Decision{}, k.err
	}
	k.keys = append(k.keys, key)
	return limiter.Decision{Allowed: true}, nil
}

const (
	tenantA = "contoso-com.mail.protection.outlook.com"
	tenantB = "fabrikam-de.mail.protection.outlook.com"
)

// Two tenants are one system: one bucket, one state, one working point.
func TestTenantsOfOneFamilyShareOneBucket(t *testing.T) {
	s := newStore(nil)
	k := &keyTaker{}
	p := New(s, k, Options{})
	ctx := context.Background()

	for _, host := range []string{tenantA, tenantB, tenantA} {
		if err := p.Acquire(ctx, host, "example.de"); err != nil {
			t.Fatalf("Acquire(%s) = %v", host, err)
		}
	}
	for _, key := range k.keys {
		if key != "@microsoft-eop" {
			t.Fatalf("a tenant drew from %q, want the family bucket", key)
		}
	}
	if n := p.Tracked(); n != 1 {
		t.Errorf("tracking %d entries for two tenants, want 1", n)
	}

	// A throttle answered by one tenant slows the whole family.
	before, _ := p.Rate(tenantB)
	p.Observe(ctx, tenantA, true)
	after, _ := p.Rate(tenantB)
	if after >= before {
		t.Errorf("tenant B's rate %v after tenant A was throttled, want below %v", after, before)
	}
	if got := s.get("rt:mx:@microsoft-eop:rate"); got == "" {
		t.Error("the working point was not persisted under the family key")
	}
	for key := range s.kv {
		if key == "rt:mx:"+tenantA+":rate" || key == "rt:mx:"+tenantB+":rate" {
			t.Errorf("a per-tenant key %q was written", key)
		}
	}
}

// A host outside every family keeps its own bucket, keyed by itself.
func TestUnmatchedHostKeepsItsOwnBucket(t *testing.T) {
	k := &keyTaker{}
	p := New(newStore(nil), k, Options{})
	if err := p.Acquire(context.Background(), "mx.example.de", "example.de"); err != nil {
		t.Fatal(err)
	}
	if len(k.keys) != 1 || k.keys[0] != "mx.example.de" {
		t.Errorf("drew from %v, want [mx.example.de]", k.keys)
	}
}

// Invariant 6 on a shared bucket: one tenant's `550 5.4.1` — directory-based
// edge blocking, a tenant setting — reaches the pacer as "not a throttle" and
// must leave the rate every other tenant runs at exactly where it was.
func TestAPolicyAnswerFromOneTenantLeavesTheFamilyRate(t *testing.T) {
	p := New(newStore(nil), &keyTaker{}, Options{})
	ctx := context.Background()
	if err := p.Acquire(ctx, tenantA, "example.de"); err != nil {
		t.Fatal(err)
	}
	before, _ := p.Rate(tenantB)
	for range 20 {
		p.Observe(ctx, tenantA, false)
	}
	after, _ := p.Rate(tenantB)
	if after < before {
		t.Errorf("family rate fell from %v to %v on policy answers", before, after)
	}
}

// Invariant 5: a family key is still a Take, and a Take that fails is no probe.
func TestFamilyKeyFailsClosed(t *testing.T) {
	down := errors.New("redis: connection refused")
	p := New(newStore(nil), &keyTaker{err: down}, Options{})
	err := p.Acquire(context.Background(), tenantA, "example.de")
	if !errors.Is(err, down) {
		t.Fatalf("Acquire with the bucket down = %v, want the store's error", err)
	}
}

// A company on Workspace has no seed of its own; it is paced as Google, not as
// a stranger (0.1-0.5/s).
func TestWorkspaceDomainGetsTheGoogleBand(t *testing.T) {
	p := New(newStore(nil), &keyTaker{}, Options{})
	if err := p.Acquire(context.Background(), "aspmx.l.google.com", "basket-dragons.de"); err != nil {
		t.Fatal(err)
	}
	google, _ := readSeed("@google")
	if got, _ := p.Rate("aspmx.l.google.com"); got != google.MaxRate {
		t.Errorf("Workspace domain starts at %v/s, want the @google seed's %v", got, google.MaxRate)
	}
	if google.MaxRate <= conservative().MaxRate {
		t.Errorf("@google seed %v is no better than a stranger's %v", google.MaxRate, conservative().MaxRate)
	}
}

// Both family seeds ship, and they are the consumer seeds they were taken from.
func TestFamilySeedsShip(t *testing.T) {
	for fam, from := range map[string]string{"@microsoft-eop": "outlook.com", "@google": "gmail.com"} {
		got, ok := readSeed(fam)
		if !ok {
			t.Fatalf("no shipped seed for %s", fam)
		}
		want, _ := readSeed(from)
		if got != want {
			t.Errorf("%s seed = %v, want %s's %v", fam, got, from, want)
		}
	}
}

// A measured family band wins over the shipped seed, and a promotion lands on
// the family key whichever tenant the operator names.
func TestFamilyBandIsMeasuredAndPromotedUnderTheKey(t *testing.T) {
	s := newStore(map[string]string{
		"limits:mx:@microsoft-eop": `{"min_rate_per_sec":0.2,"max_rate_per_sec":2,"burst":1}`,
		"limits:mx:@microsoft-eop:proposed": `{"mx_host":"@microsoft-eop","current_max_rate_per_sec":2,` +
			`"proposed_max_rate_per_sec":3,"clean_answers_at_ceiling":500}`,
	})
	p := New(s, &keyTaker{}, Options{})
	ctx := context.Background()
	if err := p.Acquire(ctx, tenantA, "example.de"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Rate(tenantA); got != 2 {
		t.Errorf("family starts at %v, want the measured band's 2", got)
	}
	if _, ok := p.Proposal(ctx, tenantB); !ok {
		t.Fatal("the family's proposal is not visible through a sibling tenant")
	}
	if _, err := p.Promote(ctx, tenantB); err != nil {
		t.Fatal(err)
	}
	if got := s.get("limits:mx:@microsoft-eop"); got == "" || got == `{"min_rate_per_sec":0.2,"max_rate_per_sec":2,"burst":1}` {
		t.Errorf("promotion did not rewrite the family band: %q", got)
	}
	if _, ok := s.kv["limits:mx:"+tenantB]; ok {
		t.Error("promotion wrote a per-tenant band")
	}
}

// A pause names the family, so the caller's retry and the operator's log point
// at the system that stood us down.
func TestPauseOnAFamilyNamesTheKey(t *testing.T) {
	s := newStore(map[string]string{
		"limits:mx:@microsoft-eop": `{"min_rate_per_sec":0.5,"max_rate_per_sec":0.5,"burst":1,"pause_seconds":600}`,
	})
	rec := &countingRecorder{}
	p := New(s, &keyTaker{}, Options{Metrics: rec})
	ctx := context.Background()
	if err := p.Acquire(ctx, tenantA, "example.de"); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx, tenantA, true) // at the floor already: stand down
	var pe *PausedError
	if err := p.Acquire(ctx, tenantB, "example.de"); !errors.As(err, &pe) {
		t.Fatalf("sibling tenant Acquire = %v, want PausedError", err)
	}
	if pe.MXHost != "@microsoft-eop" {
		t.Errorf("pause names %q, want the family", pe.MXHost)
	}
	if rec.pauses != 1 {
		t.Errorf("pauses counted %d, want 1", rec.pauses)
	}
}
