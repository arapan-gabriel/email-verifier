package iphealth

import (
	"errors"
	"strings"
)

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

// redactError returns err with zone's key replaced by `<key>`, for errors that
// name the query — a resolver's *net.DNSError carries the whole
// `2.0.0.127.<key>.combined.mail.abusix.zone` in its Name.
//
// The result is flattened to text on purpose. Keeping the original in an
// Unwrap chain would hand the key to anything that walks it, and nothing that
// receives a query error needs its type: `query` has already read IsNotFound.
// Measured 2026-09-14 on the node, with a wrong key: the dropped zone's `zone`
// field was redacted and its `error` field, on the same log line, was not.
func redactError(err error, zone string) error {
	reported := RedactZone(zone)
	if err == nil || reported == zone {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), zone, reported))
}

// RedactedZones maps RedactZone over a list, for the call sites that report a set.
func RedactedZones(zones []string) []string {
	out := make([]string, len(zones))
	for i, zone := range zones {
		out[i] = RedactZone(zone)
	}
	return out
}
