package iphealth

import (
	"context"
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
