# Service — resolver (`internal/resolver`)

A-record resolution of the supplied `mx_host`, with the SSRF guard. **Live** (plan 002); the cache
arrives with plan 004.

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
- Still to come in plan 004: the Redis cache (`dns:mx:<domain>`) with positive and negative TTLs.
