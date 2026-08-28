# Plan 002 — ssrf-guard-and-safety

**Status:** Active
**Phase:** A
**Depends on:** 001

## Goal

Make it impossible to open a socket to a private/loopback/link-local address from a
domain-controlled MX record, and lock down the "us ≠ address" verdict rules with regression tests.

## Context

The MX host is attacker-influenced (a domain owner can point MX at `127.0.0.1` or cloud metadata).
The `ds-smtp-retry` lab deliberately connects to loopback — that is the ONE behaviour we invert here.
Mirrors Data Scout's plan-024 guard. Pattern: `patterns/ssrf-guard.md`.

## Design

> **Shape under ADR-006.** MX *discovery* stays in Data Scout; the caller sends `mx_host`. That
> changes nothing about the risk: `mx_host` is still a value a domain owner chose, so it is still
> attacker-influenced and still has to be resolved and vetted here. What narrows is the scope — this
> plan guards A-record resolution of a supplied host, not MX discovery.
>
> The hole being closed is concrete: plan 001 dials `net.JoinHostPort(req.MXHost, port)` and lets
> the dialer resolve the name, so no guard can sit between the lookup and the socket. After this
> plan the prober receives an already-vetted `netip.Addr` and dials that.

- `internal/resolver` gains an SSRF guard applied to every resolved IP before it reaches the prober.
  Reject: loopback, private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`), link-local
  (`169.254/16` incl. cloud metadata, `fe80::/10`), unspecified, multicast, reserved.
- A domain whose MX resolves only to blocked IPs → `unknown` with a reason (our refusal, not the
  mailbox's absence) — never `invalid`.
- The prober can only receive vetted IPs — the guard is not a caller responsibility.
- Add regression tests for the full "us ≠ address" matrix: conn refused, TLS error, timeout, 4xx,
  5xx-before-MAIL-FROM all → `unknown`; only 5xx-on-RCPT-after-good-MAIL-FROM → `invalid`.

## Tasks

- [x] IP-range guard util + unit tests (v4 + v6 ranges)
- [x] Wire the guard into resolution so the prober only gets routable IPs
- [x] `unknown`+reason path when all MX IPs are blocked
- [x] Regression suite for the us≠address matrix (stub prober/transport)
- [x] Update `service/dns-resolver.md`, `changelog.md`

## Definition of Done

- [x] A probe of a domain whose MX points at `127.0.0.1` (stub resolver) is refused, not attempted —
      test
- [x] Cloud-metadata IP (`169.254.169.254`) blocked — test
- [x] us≠address matrix all green
- [x] gates clean; pr-checklist SSRF + us≠address items confirmed
- [ ] Status → Complete, moved, ROADMAP updated — pending manual sign-off

## Results (2026-08-28)

Gate clean: `go test -race`, `go vet`, `gofmt -l`, `golangci-lint run`.

**The hole that was closed.** Plan 001 built the target as `net.JoinHostPort(req.MXHost, port)` and
handed the *name* to the dialer, which resolved it itself. No guard could sit between that lookup
and the socket. The prober now receives vetted `netip.Addr` values and dials an IP literal, so no
second, unguarded resolution can happen underneath it.

Over HTTP, with no resolver configured — the guard is the **default**, not something a caller opts
into, so forgetting to wire it cannot produce an unguarded prober:

| `mx_host` | Result |
|---|---|
| `127.0.0.1` | `class:guarded` · `ssrf guard: 127.0.0.1 resolves to 127.0.0.1 (loopback)` |
| `169.254.169.254` | `class:guarded` · `(link-local)` — the cloud metadata endpoint |
| `10.0.0.5` | `class:guarded` · `(private)` |
| `::ffff:127.0.0.1` | `class:guarded` · `(loopback)` — a v4-mapped literal does not slip past the v4 checks |
| `localhost` (a *name*, so the DNS path) | `class:guarded` · `localhost resolves to 127.0.0.1 (loopback)` |

Every one returned `connected:false`, `accepted:null` and a reason. A test asserts the dialer was
never called at all.

From the deployed node, against a real MX, to prove the guard is not a denial of service against
ourselves: `gmail-smtp-in.l.google.com` still returned `accepted:true` for a live mailbox and
`accepted:false` `550 5.1.1` for a random one.

## Notes / decisions / deviations

This is the single most important safety plan — a bug here is either an SSRF hole or a
list-destroyer. Prefer over-blocking (a rare false `unknown`) to ever connecting inward.

**Decisions taken while building it:**

- **Deny by default.** The guard refuses anything that is not routable global unicast, rather than
  matching a list of bad ranges and allowing the rest. The named ranges in the table then cover what
  the standard-library predicates miss: carrier-grade NAT, benchmarking, the three documentation
  blocks, IETF assignments, and the v6 tunnelling prefixes (6to4, Teredo, NAT64) that can embed an
  arbitrary IPv4 address and smuggle it inward.
- **IP literals are vetted without a lookup.** `mx_host: "127.0.0.1"` must not depend on what a
  resolver chooses to do with a literal.
- **New class `ClassGuarded`**, added rather than ported — the lab connects to loopback on purpose.
  It is neither `IsTemp` nor `IsThrottle`: retrying changes nothing, and slowing down does not
  change someone's DNS record. Plan 009's `verify_probe_blocked_total{reason="ssrf"}` reads off it.
- **Resolution is `ip4` only** (invariant 3). Asking for AAAA would only produce addresses we must
  refuse to leave from, since the published identity covers the IPv4 address alone.
- **A guard refusal and a DNS failure are different errors** (`*BlockedError` vs a wrapped lookup
  error), so the caller can tell "we would not go there" from "DNS did not answer".
- The integration tests bypass the guard with an explicit `loopbackResolver`, because the fake MX
  runs on loopback. Bypassing it visibly in one named type is better than weakening the guard so
  that tests pass.
