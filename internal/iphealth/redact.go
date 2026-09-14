package iphealth

import "strings"

// Zone families whose subscription key rides *inside the query name*, as the
// first label: Abusix (`<key>.combined.mail.abusix.zone`) and Spamhaus's Data
// Query Service (`<key>.zen.dq.spamhaus.net`). Both are four labels without a
// key and five with one, which is the whole test — no list of individual zone
// names to keep current, and a family that adds a zone tomorrow is covered.
//
// A key in a zone string is a credential wearing a hostname, and this package
// hands zone strings to three places that are not the query: a log line, a
// metric label, and the `reason` that `GET /admin/ip-health` returns — which
// Data Scout's health check puts in an **email**. Redacting at the point of
// report keeps the raw zone on the wire, where it belongs, and out of
// everywhere a person reads.
var keyedZoneFamilies = []struct {
	suffix  string
	keyless int // labels when no key is present
}{
	{".mail.abusix.zone", 4},
	{".dq.spamhaus.net", 4},
}

// RedactZone replaces a keyed zone's leading label with `<key>`.
//
// A zone with no key — including the bare keyed zone itself — comes back
// unchanged: redacting what is not a secret only makes a log harder to read.
func RedactZone(zone string) string {
	for _, family := range keyedZoneFamilies {
		if !strings.HasSuffix(zone, family.suffix) {
			continue
		}
		labels := strings.Split(zone, ".")
		if len(labels) <= family.keyless {
			return zone
		}
		return "<key>." + strings.Join(labels[1:], ".")
	}
	return zone
}

// RedactedZones maps RedactZone over a list, for the call sites that report a set.
func RedactedZones(zones []string) []string {
	out := make([]string, len(zones))
	for i, zone := range zones {
		out[i] = RedactZone(zone)
	}
	return out
}
