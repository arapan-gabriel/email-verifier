// Package resolver turns a supplied MX host into addresses that are safe to
// connect to, and refuses the ones that are not.
//
// The MX host is attacker-influenced data: a domain owner can point their MX
// record at 127.0.0.1, at 169.254.169.254 (cloud metadata), or into a private
// range, and a naive prober would then open a socket from inside our network to
// an internal target. That is a server-side request forgery, and it is the one
// behaviour ../ds-smtp-retry does on purpose that this service must invert
// (invariant 2, docs/03-engineering/patterns/ssrf-guard.md).
package resolver

import (
	"fmt"
	"net/netip"
)

// blockedPrefix is a range no probe may ever reach, with the reason it is
// listed. The netip predicates below cover the classics; this table covers what
// they miss and what a reader would otherwise have to know by heart.
type blockedPrefix struct {
	prefix netip.Prefix
	reason string
}

var blockedPrefixes = buildBlocked([]struct{ cidr, reason string }{
	{"0.0.0.0/8", "this network"},
	{"100.64.0.0/10", "carrier-grade NAT"},
	{"192.0.0.0/24", "IETF protocol assignments"},
	{"192.0.2.0/24", "documentation (TEST-NET-1)"},
	{"192.88.99.0/24", "6to4 relay anycast"},
	{"198.18.0.0/15", "benchmarking"},
	{"198.51.100.0/24", "documentation (TEST-NET-2)"},
	{"203.0.113.0/24", "documentation (TEST-NET-3)"},
	{"240.0.0.0/4", "reserved"},
	{"::/96", "IPv4-compatible IPv6"},
	// A v4-mapped v6 literal would otherwise smuggle a private v4 address past
	// the v4 checks entirely.
	{"::ffff:0:0/96", "IPv4-mapped IPv6"},
	{"64:ff9b::/96", "NAT64"},
	{"100::/64", "discard-only"},
	{"2001::/32", "Teredo"},
	{"2001:20::/28", "ORCHIDv2"},
	{"2001:db8::/32", "documentation"},
	{"2002::/16", "6to4 — embeds an arbitrary IPv4 address"},
})

func buildBlocked(in []struct{ cidr, reason string }) []blockedPrefix {
	out := make([]blockedPrefix, 0, len(in))
	for _, e := range in {
		out = append(out, blockedPrefix{netip.MustParsePrefix(e.cidr), e.reason})
	}
	return out
}

// Blocked reports whether addr must never be connected to, and why.
//
// It is deliberately a deny-by-default check: anything that is not a routable
// global unicast address is refused. Over-blocking costs a rare unnecessary
// "not attempted"; under-blocking is an SSRF hole.
func Blocked(addr netip.Addr) (bool, string) {
	if !addr.IsValid() {
		return true, "invalid address"
	}
	// Zoned addresses (fe80::1%eth0) are link-local by construction.
	if addr.Zone() != "" {
		return true, "zoned address"
	}

	switch {
	case addr.IsUnspecified():
		return true, "unspecified"
	case addr.IsLoopback():
		return true, "loopback"
	case addr.IsPrivate():
		return true, "private"
	case addr.IsLinkLocalUnicast():
		// 169.254.169.254 — the cloud metadata endpoint — lives here.
		return true, "link-local"
	case addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return true, "multicast"
	}

	for _, b := range blockedPrefixes {
		if b.prefix.Contains(addr) {
			return true, b.reason
		}
	}

	// Anything left that is not global unicast is not a mail server.
	if !addr.IsGlobalUnicast() {
		return true, "not global unicast"
	}
	return false, ""
}

// BlockedError is the refusal to open a socket to a particular address.
type BlockedError struct {
	Host   string
	Addr   netip.Addr
	Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("ssrf guard: %s resolves to %s (%s)", e.Host, e.Addr, e.Reason)
}
