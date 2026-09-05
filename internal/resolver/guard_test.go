package resolver

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// The table this package exists for. Over-blocking costs a rare unnecessary
// "not attempted"; under-blocking is an SSRF hole (invariant 2).
func TestBlocked(t *testing.T) {
	blocked := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"127.10.20.30", "loopback is a whole /8"},
		{"::1", "loopback v6"},
		{"10.0.0.1", "private"},
		{"172.16.0.1", "private"},
		{"172.31.255.255", "private, top of range"},
		{"192.168.1.1", "private"},
		{"fd00::1", "unique local"},
		{"169.254.169.254", "cloud metadata"},
		{"169.254.0.1", "link-local"},
		{"fe80::1", "link-local v6"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified v6"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "multicast v6"},
		{"255.255.255.255", "broadcast"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"192.0.0.1", "IETF assignments"},
		{"192.0.2.1", "documentation"},
		{"198.18.0.1", "benchmarking"},
		{"198.51.100.1", "documentation"},
		{"203.0.113.1", "documentation"},
		{"240.0.0.1", "reserved"},
		{"::ffff:127.0.0.1", "v4-mapped loopback must not slip past the v4 checks"},
		{"::ffff:10.0.0.1", "v4-mapped private"},
		{"2001:db8::1", "documentation v6"},
		{"2002:7f00:1::", "6to4 embedding 127.0.0.1"},
		{"2001::1", "Teredo"},
		{"64:ff9b::7f00:1", "NAT64 embedding loopback"},
	}
	for _, tc := range blocked {
		t.Run("blocked/"+tc.addr, func(t *testing.T) {
			got, reason := Blocked(netip.MustParseAddr(tc.addr))
			if !got {
				t.Errorf("Blocked(%s) = false; %s", tc.addr, tc.why)
			}
			if reason == "" {
				t.Errorf("Blocked(%s) gave no reason", tc.addr)
			}
		})
	}

	// Real mail servers must still be reachable, or the guard is a denial of
	// service against ourselves.
	for _, addr := range []string{
		"142.250.150.27", "8.8.8.8", "92.222.87.97", "1.1.1.1",
		"2a00:1450:4001:c21::1b",
	} {
		t.Run("allowed/"+addr, func(t *testing.T) {
			if got, reason := Blocked(netip.MustParseAddr(addr)); got {
				t.Errorf("Blocked(%s) = true (%s); a routable address was refused", addr, reason)
			}
		})
	}
}

func TestBlockedRejectsInvalidAndZoned(t *testing.T) {
	if got, _ := Blocked(netip.Addr{}); !got {
		t.Error("the zero Addr was allowed")
	}
	zoned := netip.MustParseAddr("fe80::1").WithZone("eth0")
	if got, _ := Blocked(zoned); !got {
		t.Error("a zoned address was allowed")
	}
}

func stub(addrs ...string) LookupFunc {
	return func(context.Context, string, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func TestResolveFiltersBlockedAnswers(t *testing.T) {
	r := New(Options{Lookup: stub("127.0.0.1", "142.250.150.27", "10.0.0.1")})
	got, err := r.Resolve(t.Context(), "mx.example.test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].String() != "142.250.150.27" {
		t.Errorf("Resolve = %v, want only the routable answer", got)
	}
}

// The attack shape: a domain owner points MX only at something internal.
func TestResolveRefusesWhenEveryAnswerIsBlocked(t *testing.T) {
	r := New(Options{Lookup: stub("127.0.0.1", "169.254.169.254")})
	_, err := r.Resolve(t.Context(), "evil.example.test")
	if err == nil {
		t.Fatal("Resolve returned no error for an all-internal answer")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("error is %T, want *BlockedError so the caller can tell refusal from DNS failure", err)
	}
	if be.Host != "evil.example.test" || be.Reason == "" {
		t.Errorf("BlockedError = %+v, want host and reason set", be)
	}
}

// An IP literal must be vetted directly: a resolver that echoes the literal
// back would otherwise walk it straight past the guard.
func TestResolveVetsLiteralsWithoutLookup(t *testing.T) {
	called := false
	r := New(Options{Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
		called = true
		return nil, errors.New("must not be called")
	}})

	if _, err := r.Resolve(t.Context(), "127.0.0.1"); err == nil {
		t.Error("a loopback literal was accepted")
	}
	if got, err := r.Resolve(t.Context(), "142.250.150.27"); err != nil || len(got) != 1 {
		t.Errorf("a routable literal was refused: %v %v", got, err)
	}
	if called {
		t.Error("a literal triggered a DNS lookup")
	}
}

func TestResolveReportsDNSFailureDistinctly(t *testing.T) {
	boom := errors.New("no such host")
	r := New(Options{Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, boom
	}})
	_, err := r.Resolve(t.Context(), "nx.example.test")
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the DNS error wrapped", err)
	}
	var be *BlockedError
	if errors.As(err, &be) {
		t.Error("a DNS failure was reported as a guard refusal")
	}
}

// Invariant 3: resolving AAAA would only produce addresses we must refuse to
// leave from, since the published identity covers the IPv4 address alone.
func TestResolverAsksForIPv4Only(t *testing.T) {
	var asked string
	r := New(Options{Lookup: func(_ context.Context, network, _ string) ([]netip.Addr, error) {
		asked = network
		return []netip.Addr{netip.MustParseAddr("142.250.150.27")}, nil
	}})
	if _, err := r.Resolve(t.Context(), "mx.example.test"); err != nil {
		t.Fatal(err)
	}
	if asked != "ip4" {
		t.Errorf("looked up %q, want ip4", asked)
	}
}

func TestResolveRejectsEmptyHost(t *testing.T) {
	if _, err := New(Options{}).Resolve(t.Context(), ""); err == nil {
		t.Error("empty host accepted")
	}
}
