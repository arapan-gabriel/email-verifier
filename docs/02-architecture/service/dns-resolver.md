# Service — resolver (`internal/resolver`)

A-record resolution of the supplied `mx_host`, with the SSRF guard and an in-process cache.
**Live** (plans 002 and 004).

- MX *discovery* is Data Scout's (layer 5, ADR-006). This package resolves the `mx_host` the caller
  sends — still attacker-influenced data, because a domain owner chose that record.
- **SSRF guard on every address, deny by default**:
  `docs/03-engineering/patterns/ssrf-guard.md` (invariant 2).
- **`ip4` only** (invariant 3) — the published sending identity covers the IPv4 address alone.
- IP literals are vetted directly, without a lookup.
- The prober takes `netip.Addr`, never a hostname, so the guard cannot end up on the wrong side of
  the resolution. It defaults to a guarded resolver, so forgetting to wire one cannot produce an
  unguarded prober.
- A refusal is `*BlockedError`, distinct from a DNS failure, and surfaces as `class: guarded` with
  `connected:false` and `accepted:null` — our refusal, never the mailbox's absence.
- **Configurable resolvers** (`dns.servers`) and a resolution timeout of its own, so a slow or
  misbehaving stub cannot spend the probe's whole budget and an operator can move this service off
  the host's resolver without touching the rest of the host.
- **An in-process TTL cache**, positive and negative, size-capped. Not Redis: the node already runs
  a caching resolver, so a Redis round trip to avoid a cached lookup would be slower. It exists so a
  500-domain bulk job at one provider is not 500 identical queries wherever `resolv.conf` points
  straight at a public resolver.
- **Only vetted results are cached.** Caching the raw answer would be a way to smuggle a refused
  address past the guard. Refusals are cached too, on a shorter TTL — a domain pointing its MX
  inward should cost one lookup, not one per request.
- IP literals are neither looked up nor cached.
- No `miekg/dns`: the one thing the standard library cannot give is the record TTL, and a fixed
  conservative TTL is adequate for the A records of MX hosts, which change rarely.
