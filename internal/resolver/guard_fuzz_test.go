package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// FuzzGuardBlocked is invariant 2 as a property (plan 027): any address netip
// itself reports as loopback, private, link-local, unspecified or multicast —
// as a v4 literal, as v4-mapped v6, or as native v6 — is blocked. The guard is
// deny-by-default, so the property is one-way: some addresses netip calls
// global are still refused (documentation ranges, 6to4, …), and that is fine.
func FuzzGuardBlocked(f *testing.F) {
	for _, s := range [][]byte{
		{127, 0, 0, 1}, {10, 1, 2, 3}, {169, 254, 169, 254}, {0, 0, 0, 0}, {224, 0, 0, 1},
		{192, 168, 1, 1}, {172, 16, 0, 1}, {142, 250, 150, 27}, {100, 64, 0, 1},
		netip.MustParseAddr("::1").AsSlice(), netip.MustParseAddr("fe80::1").AsSlice(),
		netip.MustParseAddr("fc00::1").AsSlice(), netip.MustParseAddr("ff02::1").AsSlice(),
		netip.MustParseAddr("::ffff:127.0.0.1").AsSlice(), netip.MustParseAddr("2607:f8b0::1").AsSlice(),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		addr, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		forms := []netip.Addr{addr}
		if addr.Is4() {
			forms = append(forms, netip.AddrFrom16(addr.As16())) // ::ffff:a.b.c.d
		}
		if addr.Is4In6() {
			forms = append(forms, addr.Unmap())
		}
		for _, a := range forms {
			inner := a.Unmap()
			dangerous := inner.IsLoopback() || inner.IsPrivate() || inner.IsLinkLocalUnicast() ||
				inner.IsUnspecified() || inner.IsMulticast() || inner.IsLinkLocalMulticast() ||
				inner.IsInterfaceLocalMulticast()
			if blocked, _ := Blocked(a); dangerous && !blocked {
				t.Fatalf("Blocked(%s) = false for an address netip calls internal (invariant 2)", a)
			}
			// Through Resolve too, as a literal: the guard must not be
			// bypassable by handing the prober an IP instead of a name.
			if dangerous {
				if _, err := New(Options{}).Resolve(context.Background(), a.String()); err == nil {
					t.Fatalf("Resolve(%q) let an internal literal through", a)
				}
			}
		}
	})
}

func TestBlockedErrorNamesTheHostAndReason(t *testing.T) {
	_, err := New(Options{Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}}).Resolve(t.Context(), "inward.example")
	var be *BlockedError
	if !errors.As(err, &be) || !strings.Contains(err.Error(), "inward.example") || !strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v, want a BlockedError naming host and reason", err)
	}
	if blocked, reason := Blocked(netip.Addr{}); !blocked || reason != "invalid address" {
		t.Errorf("zero Addr = %v %q", blocked, reason)
	}
	if blocked, reason := Blocked(netip.MustParseAddr("fe80::1%eth0")); !blocked || reason != "zoned address" {
		t.Errorf("zoned = %v %q", blocked, reason)
	}
}

// Concurrent fill and evict under -race (plan 027, Design 8): many goroutines
// resolving more hosts than the cache holds.
func TestConcurrentCacheFillAndEvict(t *testing.T) {
	r := New(Options{CacheSize: 8, Lookup: func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if strings.HasPrefix(host, "bad") {
			return nil, errors.New("SERVFAIL")
		}
		return []netip.Addr{netip.MustParseAddr("142.250.150.27")}, nil
	}})
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				host := fmt.Sprintf("mx%d.example", (g*7+i)%40)
				if i%5 == 0 {
					host = "bad" + host
				}
				_, _ = r.Resolve(context.Background(), host)
			}
		}()
	}
	wg.Wait()
	if n := r.cache.len(); n > 8 {
		t.Errorf("cache holds %d entries, cap 8", n)
	}
}

// When the cache is full of live entries, eviction still makes room (by
// dropping one), and when some have expired it drops those first.
func TestEvictionWhenFullAndWhenExpired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCache(time.Minute, time.Minute, 2)
		c.put("a", nil, nil)
		c.put("b", nil, nil)
		c.put("c", nil, nil) // all live: one arbitrary entry goes
		if c.len() != 2 {
			t.Fatalf("len = %d, want 2", c.len())
		}
		time.Sleep(2 * time.Minute)
		c.put("d", nil, nil) // both expired: both go, d stays
		if c.len() != 1 {
			t.Errorf("len = %d, want 1 after expired entries were dropped", c.len())
		}
	})
}

// With servers configured the resolver dials them in order and falls through
// a dead one; with none it is the process resolver.
func TestResolverForServers(t *testing.T) {
	if resolverFor(nil, 0) != net.DefaultResolver {
		t.Error("no servers must mean the process resolver")
	}
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	res := resolverFor([]string{"127.0.0.1:1", pc.LocalAddr().String()}, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	// Nothing answers, so the lookup fails; the point is that it tried and
	// returned rather than hanging past the context.
	if _, err := res.LookupNetIP(ctx, "ip4", "mx.example."); err == nil {
		t.Error("a lookup through silent servers succeeded")
	}
}

func TestMXLookupFailure(t *testing.T) {
	r := New(Options{LookupMX: func(context.Context, string) ([]*net.MX, error) { return nil, errors.New("SERVFAIL") }})
	if _, err := r.MX(t.Context(), "example.test"); err == nil {
		t.Error("an MX lookup failure was swallowed")
	}
	if _, err := r.MX(t.Context(), ""); err == nil {
		t.Error("an empty domain was accepted")
	}
}
