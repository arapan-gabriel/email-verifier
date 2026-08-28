# Plan 004 — dns-resolver-and-cache

**Status:** Active
**Phase:** A
**Depends on:** 002

> **Narrowed twice.** ADR-006 moved MX *discovery* to Data Scout: it already resolves MX to group
> addresses by domain and sends the chosen `mx_host`. Plan 002 then built the resolution and the
> SSRF guard. What is genuinely left is control over *which* resolver answers, and not depending on
> the host having a caching one.

## Goal

Stop the resolver being whatever `/etc/resolv.conf` happens to say, and stop a bulk job re-asking
for the same MX host once per recipient domain.

## Context

`internal/resolver` (plan 002) resolves the supplied `mx_host` to IPv4 through the guard, using the
process resolver. On the deployed node that is `systemd-resolved` on `127.0.0.53` — the local stub
the RUNBOOK warns about. For A records it behaves and it caches; the warning bites on DNSBL lookups,
which is plan 010's problem, not this one.

Two things still need fixing:

- **The resolver is not ours to choose.** An operator cannot point this service at a resolver that
  can answer DNSBL queries, or away from a stub that is misbehaving, without editing the host's
  `resolv.conf` and affecting everything else on it.
- **Resolution is unbounded.** It inherits only the HTTP request's deadline, so a slow resolver
  eats the probe's whole budget before a socket is ever opened.

## Design

- `internal/resolver` gains configurable servers and its own timeout. Implemented with the standard
  library's `net.Resolver` and a custom `Dial`; **no `miekg/dns`.** The one thing stdlib cannot give
  is the record TTL, and a fixed conservative TTL is adequate for the A records of MX hosts, which
  change rarely. Revisit only if TTL-honouring turns out to matter.
- Empty server list keeps the process resolver, so the default stays "whatever the host does".
- **A small in-process TTL cache**, positive and negative, with a size cap. Its purpose is *not*
  speed — the deployed node already runs a caching resolver, and going to Redis to avoid a lookup
  the OS has cached would be slower, not faster. Its purpose is that the service must not fall over
  when deployed somewhere `resolv.conf` points straight at a public resolver and a 500-domain bulk
  job means 500 identical queries.
- **Guard refusals are cached too.** A domain pointing its MX inward should cost one lookup, not one
  per request.

### Deviation: no `dns:mx:<domain>` in Redis

The Redis contract reserved a key for this. It is dropped: a DNS answer is not operational state
that has to be shared between nodes, and a Redis round trip to avoid a lookup the local resolver
already caches would make the hot path slower rather than faster. Redis stays for what genuinely
must be shared — the rate budget, the bands, IP health.

## Tasks

- [x] Configurable resolver servers + resolution timeout in `internal/resolver` and `internal/config`
- [x] In-process TTL cache: positive answers, negative answers, guard refusals; size cap
- [x] Cache is bypassable for IP literals (nothing to cache) and honours the context deadline
- [x] Unit tests with a stub lookup: hit, miss, expiry, negative caching, eviction
- [x] Remove `dns:mx:<domain>` from `redis-contract.md` and record why
- [x] Update `service/dns-resolver.md`, `operations/dns.md`, changelog

## Definition of Done

- [x] A repeated lookup is served from the cache — asserted by counting lookups, not by timing
- [x] A refused host is refused from the cache without a second lookup
- [x] Entries expire; the cache does not grow without bound
- [x] Pointing the service at a dead resolver fails resolution rather than hanging
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated — pending manual sign-off

## Results (2026-08-28)

Gate clean. Every cache assertion counts *lookups performed* rather than elapsed time, because
timing proves nothing about whether a cache was consulted.

| Check | Result |
|---|---|
| 10 resolutions of one host | 1 lookup |
| 5 refusals of an inward-pointing host | 1 lookup |
| 4 resolutions of a host that does not resolve | 1 lookup |
| Entry expiry at the positive TTL | re-looked-up only after 5m, under `synctest` |
| Refusal held on the shorter negative TTL | re-looked-up after 90s, not held for the positive 10m |
| 200 distinct hosts into a cap of 16 | 16 entries held |
| IP literals | 0 lookups, 0 cache entries |
| Hanging resolver with a 50ms timeout | returns an error promptly instead of spending the probe's budget |
| Dead configured server | fails rather than silently falling back to the host's resolver |

## Notes / decisions / deviations

**What this plan turned out not to need.** As written it was about MX discovery, priority sorting,
implicit-MX fallback and a "no mail server" verdict. ADR-006 moved all of that to Data Scout, which
resolves MX to group addresses by domain before it calls here. Plan 002 then built resolution and
the guard. Rather than implement the original tasks against a boundary that had moved, the plan was
rewritten to its real remainder — resolver control and not depending on the host having a cache —
and the `dns:mx:<domain>` Redis key was dropped with the reasoning recorded in the contract.

Keep the guard as the resolver's responsibility (plan 002) — resolution and vetting are one step, so
a caller cannot obtain an unvetted IP, and the cache must therefore store *vetted* results only.
Caching before the guard would be a way to smuggle a refused address back in.
