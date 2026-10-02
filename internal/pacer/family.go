package pacer

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// A hostname is not a receiving system. Every Microsoft 365 tenant presents its
// own MX (`<tenant>.mail.protection.outlook.com`), so keyed by hostname Exchange
// Online Protection had one bucket per tenant and no shared limit at all — while
// EOP itself counts us per sending IP across every tenant. PaceKey maps the
// hostnames of one known system onto one key, so the bucket, the band and the
// AIMD state are that system's, not each tenant's (plan 026, invariant 4).
//
//go:embed families.json
var familiesJSON []byte

type family struct {
	Suffix string `json:"suffix"`
	Key    string `json:"key"`
	// MultiDomain says the system answers RCPTs for several of its domains in
	// one session (plan 033). Measured per system, never assumed.
	MultiDomain bool `json:"multi_domain"`
}

// families is parsed once at start-up. The file ships inside the binary, so a
// malformed table is a build defect and fails loudly rather than pacing every
// tenant alone again.
var families = mustFamilies(familiesJSON)

// multiDomain is the set of keys whose system may be asked about several domains
// in one session (plan 033).
var multiDomain = multiDomainKeys(families)

func mustFamilies(raw []byte) []family {
	var doc struct {
		Families []family `json:"families"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(fmt.Sprintf("pacer: families.json: %v", err))
	}
	multi := map[string]bool{}
	for i, f := range doc.Families {
		f.Suffix = normaliseHost(f.Suffix)
		if f.Suffix == "" || !strings.HasPrefix(f.Key, "@") || len(f.Key) < 2 {
			panic(fmt.Sprintf("pacer: families.json entry %d: want a suffix and an @key, got %+v", i, f))
		}
		// A system either answers for several domains or it does not; a key
		// whose entries disagree would group or not by which MX name the
		// resolver happened to return.
		if seen, ok := multi[f.Key]; ok && seen != f.MultiDomain {
			panic(fmt.Sprintf("pacer: families.json entry %d: multi_domain disagrees with an earlier %s entry", i, f.Key))
		}
		multi[f.Key] = f.MultiDomain
		doc.Families[i] = f
	}
	return doc.Families
}

func multiDomainKeys(fs []family) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		if f.MultiDomain {
			out[f.Key] = true
		}
	}
	return out
}

func normaliseHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// PaceKey is the key an MX host is paced under: a family key such as
// `@microsoft-eop` for a host of a known system, otherwise the host itself —
// exactly the behaviour before plan 026.
//
// A suffix matches on a label boundary only, so `evil-outlook.com` and
// `mail.protection.outlook.com.attacker.net` stay their own hosts. A value that
// is already a key is returned unchanged, so an operator can name a family
// directly (`POST /admin/bands/promote` with `@google`).
func PaceKey(mxHost string) string {
	if strings.HasPrefix(mxHost, "@") {
		return mxHost
	}
	h := normaliseHost(mxHost)
	for _, f := range families {
		if h == f.Suffix || strings.HasSuffix(h, "."+f.Suffix) {
			return f.Key
		}
	}
	return mxHost
}

// MultiDomain returns the pace key of mxHost and whether its system may be asked
// about several domains in one session (plan 033). An unlisted host is its own
// key and never groups: whether a receiver answers for a foreign domain is
// measured, not assumed.
func MultiDomain(mxHost string) (key string, ok bool) {
	key = PaceKey(mxHost)
	return key, multiDomain[key]
}
