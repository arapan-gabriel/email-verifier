package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	// Lookup defaults to the process resolver.
	Lookup LookupFunc
	// Network is "ip4" — invariant 3. The sending identity is published for the
	// IPv4 address only, so resolving AAAA would only produce addresses we must
	// refuse to leave from.
	Network string
}

// Resolver resolves a host and vets every answer.
type Resolver struct {
	lookup  LookupFunc
	network string
}

// New returns a Resolver. It never fails: every option has a default.
func New(opts Options) *Resolver {
	r := &Resolver{lookup: opts.Lookup, network: opts.Network}
	if r.lookup == nil {
		r.lookup = net.DefaultResolver.LookupNetIP
	}
	if r.network == "" {
		r.network = "ip4"
	}
	return r
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

	addrs, err := r.lookup(ctx, r.network, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: %w", host, ErrNoRoutableAddress)
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
		// this guard exists for, so it is reported as the refusal it is.
		return nil, first
	}
	return out, nil
}
