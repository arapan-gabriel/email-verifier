package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// ErrNoRoutableAddress means the host resolved, but nothing it resolved to may
// be connected to. It is a refusal by us, never a statement about a mailbox:
// callers must surface it as "not attempted" (invariant 1).
var ErrNoRoutableAddress = errors.New("no routable address")

// LookupFunc resolves a host to addresses. Declared here so tests drive
// resolution without DNS (ENGINEERING-STANDARDS §2); net.Resolver.LookupNetIP
// satisfies it.
type LookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// Options configures a Resolver.
type Options struct {
	// Lookup overrides resolution entirely; tests use it. When nil, a resolver
	// is built from Servers.
	Lookup LookupFunc
	// Servers are "host:port" DNS servers to query. Empty means the process
	// resolver — whatever /etc/resolv.conf says, which on the deployed node is
	// systemd-resolved. Setting it lets an operator move this service off a
	// misbehaving stub without touching the rest of the host.
	Servers []string
	// Timeout bounds one resolution. Without it, resolution inherits only the
	// HTTP request deadline and a slow resolver eats the probe's whole budget
	// before a socket is ever opened.
	Timeout time.Duration
	// Network is "ip4" — invariant 3. The sending identity is published for the
	// IPv4 address only, so resolving AAAA would only produce addresses we must
	// refuse to leave from.
	Network string
	// CacheTTL and NegativeTTL bound how long answers and refusals are held.
	CacheTTL    time.Duration
	NegativeTTL time.Duration
	// CacheSize caps the number of hosts remembered.
	CacheSize int
}

// Resolver resolves a host and vets every answer.
type Resolver struct {
	lookup  LookupFunc
	network string
	timeout time.Duration
	cache   *cache
}

// New returns a Resolver. It never fails: every option has a default.
func New(opts Options) *Resolver {
	r := &Resolver{
		lookup:  opts.Lookup,
		network: opts.Network,
		timeout: opts.Timeout,
		cache:   newCache(opts.CacheTTL, opts.NegativeTTL, opts.CacheSize),
	}
	if r.lookup == nil {
		r.lookup = resolverFor(opts.Servers, opts.Timeout).LookupNetIP
	}
	if r.network == "" {
		r.network = "ip4"
	}
	if r.timeout <= 0 {
		r.timeout = 5 * time.Second
	}
	return r
}

// resolverFor builds a net.Resolver aimed at the configured servers, or the
// process resolver when none are given.
func resolverFor(servers []string, timeout time.Duration) *net.Resolver {
	if len(servers) == 0 {
		return net.DefaultResolver
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			var lastErr error
			for _, s := range servers {
				c, err := d.DialContext(ctx, network, s)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}

// Resolve returns the addresses of host that are safe to connect to.
//
// An IP literal is vetted directly rather than looked up — otherwise
// `mx_host: "127.0.0.1"` would walk straight past the guard on a resolver that
// happily returns the literal back.
//
// It returns a *BlockedError when everything the host resolved to is refused,
// so the caller can tell "we would not go there" from "DNS did not answer".
func (r *Resolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if host == "" {
		return nil, errors.New("resolver: host is empty")
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		if blocked, reason := Blocked(addr); blocked {
			return nil, &BlockedError{Host: host, Addr: addr, Reason: reason}
		}
		return []netip.Addr{addr}, nil
	}

	if e, ok := r.cache.get(host); ok {
		return e.addrs, e.err
	}

	// A slow resolver must not spend the probe's whole budget.
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	addrs, err := r.lookup(ctx, r.network, host)
	if err != nil {
		err = fmt.Errorf("resolve %s: %w", host, err)
		r.cache.put(host, nil, err)
		return nil, err
	}
	if len(addrs) == 0 {
		err := fmt.Errorf("resolve %s: %w", host, ErrNoRoutableAddress)
		r.cache.put(host, nil, err)
		return nil, err
	}

	var out []netip.Addr
	var first *BlockedError
	for _, a := range addrs {
		a = a.Unmap()
		blocked, reason := Blocked(a)
		if !blocked {
			out = append(out, a)
			continue
		}
		if first == nil {
			first = &BlockedError{Host: host, Addr: a, Reason: reason}
		}
	}
	if len(out) == 0 {
		// A host that resolves *only* to refused addresses is the attack shape
		// this guard exists for, so it is reported as the refusal it is — and
		// remembered, so it costs one lookup rather than one per request.
		r.cache.put(host, nil, first)
		return nil, first
	}
	// Only vetted addresses are cached: storing the raw answer would be a way
	// to smuggle a refused address back in past the guard.
	r.cache.put(host, out, nil)
	return out, nil
}
