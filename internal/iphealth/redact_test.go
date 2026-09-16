package iphealth

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestRedactZone(t *testing.T) {
	cases := map[string]string{
		"sk_abc123.combined.mail.abusix.zone": "<key>.combined.mail.abusix.zone",
		"KEY.zen.dq.spamhaus.net":             "<key>.zen.dq.spamhaus.net",
		"combined.mail.abusix.zone":           "combined.mail.abusix.zone",
		"zen.spamhaus.org":                    "zen.spamhaus.org",
		"bl.spamcop.net":                      "bl.spamcop.net",
		"":                                    "",
	}
	for in, want := range cases {
		if got := RedactZone(in); got != want {
			t.Errorf("RedactZone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactZoneKeepsUnkeyedZonesReadable(t *testing.T) {
	// A zone family we do not know is left alone: inventing a `<key>` where
	// there is none would hide the zone a reader is trying to identify.
	for _, zone := range []string{"psbl.surriel.com", "b.barracudacentral.org"} {
		if got := RedactZone(zone); got != zone {
			t.Errorf("RedactZone(%q) = %q, want it unchanged", zone, got)
		}
	}
}

// The other place a zone reaches a person: the reason a zone was dropped, which
// main.go logs. Measured 2026-09-14 on the node with a wrong key — `zone` was
// redacted and `error`, on the same line, printed the key twice: once from our
// own message and once inside the resolver's *net.DNSError.
func TestADroppedZoneNeverCarriesItsKey(t *testing.T) {
	const keyed = "sk_secret.combined.mail.abusix.zone"
	listed := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	servfail := func(host string) error {
		return &net.DNSError{Err: "server misbehaving", Name: host, Server: "127.0.0.53:53", IsTemporary: true}
	}
	// Every way the self-test can fail a zone, answered for the keyed zone only.
	cases := map[string]func(host string) ([]netip.Addr, error){
		"test point unreachable": func(host string) ([]netip.Addr, error) {
			return nil, servfail(host)
		},
		"test point not listed": func(string) ([]netip.Addr, error) {
			return nil, nil
		},
		"clean point unreachable": func(host string) ([]netip.Addr, error) {
			if strings.HasPrefix(host, "2.0.0.127.") {
				return listed, nil
			}
			return nil, servfail(host)
		},
		"clean point listed": func(string) ([]netip.Addr, error) {
			return listed, nil
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
				if strings.HasSuffix(host, keyed) {
					return answer(host)
				}
				if host == "2.0.0.127.zen.spamhaus.org" {
					return listed, nil
				}
				return nil, nil
			}

			h := New(Options{IP: "92.222.87.97", Zones: []string{"zen.spamhaus.org", keyed}, Lookup: lookup, Store: newStore()})
			result, err := h.SelfTest(t.Context())
			if err != nil {
				t.Fatalf("SelfTest: %v", err)
			}
			if len(result.Dropped) != 1 {
				t.Fatalf("dropped = %+v, want the keyed zone alone", result.Dropped)
			}
			reason := result.Dropped[0].Reason.Error()
			if strings.Contains(reason, "sk_secret") {
				t.Errorf("the subscription key reached the drop reason: %q", reason)
			}
			if !strings.Contains(reason, "<key>.combined.mail.abusix.zone") {
				t.Errorf("reason = %q, want it to still name the zone, redacted", reason)
			}

			// With no zone surviving, the same reason is wrapped into the error
			// that disables checking — the other string main.go logs.
			alone := New(Options{IP: "92.222.87.97", Zones: []string{keyed}, Lookup: lookup, Store: newStore()})
			if _, err := alone.SelfTest(t.Context()); err == nil {
				t.Fatal("a keyed zone that fails its self-test passed")
			} else if strings.Contains(err.Error(), "sk_secret") {
				t.Errorf("the subscription key reached the disabling error: %q", err)
			}
		})
	}
}

func TestAKeyNeverReachesTheReport(t *testing.T) {
	// The end-to-end property, at the boundary that matters: `reason` is what
	// `GET /admin/ip-health` returns and what Data Scout's health check emails.
	keyed := "sk_secret.combined.mail.abusix.zone"
	h := healthy(func(_ context.Context, name string) ([]netip.Addr, error) {
		if strings.HasPrefix(name, "1.0.0.127.") {
			return nil, nil // the "must not be listed" test point
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
	}, newStore())
	h.zones = []string{keyed}
	h.trusted = true

	rep, err := h.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rep.Burned {
		t.Fatal("the fake resolver listed the IP; the report should say so")
	}
	if strings.Contains(rep.Reason, "sk_secret") {
		t.Errorf("the subscription key reached `reason`: %q", rep.Reason)
	}
	if _, ok := rep.Listed["<key>.combined.mail.abusix.zone"]; !ok {
		t.Errorf("Listed is keyed by the raw zone: %v", rep.Listed)
	}
}

// The caveat is the third string main.go logs about a zone (plan 023), and it
// reaches the journal exactly like the other two. A keyed zone that lists the
// clean point is not hypothetical: Abusix's own zones are auto-populated too.
func TestAKeptZonesCaveatNeverCarriesItsKey(t *testing.T) {
	const keyed = "sk_secret.combined.mail.abusix.zone"
	listed := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	h := New(Options{
		IP:    "92.222.87.97",
		Zones: []string{keyed},
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			// Test point and clean point both listed; the documentation
			// addresses are not, so the zone survives with a caveat.
			if strings.HasPrefix(host, "2.0.0.127.") || strings.HasPrefix(host, "1.0.0.127.") {
				return listed, nil
			}
			return nil, nil
		},
		Store: newStore(),
	})

	result, err := h.SelfTest(t.Context())
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(result.Caveats) != 1 {
		t.Fatalf("caveats = %+v, want the keyed zone kept with one", result.Caveats)
	}
	detail := result.Caveats[0].Reason.Error()
	if strings.Contains(detail, "sk_secret") {
		t.Errorf("the subscription key reached the caveat: %q", detail)
	}
	if !strings.Contains(detail, "<key>.combined.mail.abusix.zone") {
		t.Errorf("caveat = %q, want it to still name the zone, redacted", detail)
	}
}
