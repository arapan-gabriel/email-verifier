# Plan 004 — dns-resolver-and-cache

**Status:** Planned
**Phase:** A
**Depends on:** 002

## Goal

A proper MX/A resolver with caching, no-MX fallback, and a clean "no mail server" verdict — so the
prober always targets the right endpoint and never mis-reports a domain with no mail as `invalid`.

## Context

`002` put the SSRF guard in the resolver. This plan builds the resolution itself. The lab resolves
via the OS/lab DNS; production needs its own resolver config (the RUNBOOK notes a broken local stub
breaks DNSBL and MX lookups). Component doc: `service/dns-resolver.md`.

## Design

- `internal/resolver` — MX lookup (sorted by priority), A/AAAA for the chosen host, all IPs through
  the SSRF guard (002).
- **No-MX fallback:** a domain with an A record but no MX uses implicit MX (the A host). A domain
  with neither → verdict `unknown` / "no mail server" (never `invalid` — the mailbox question was
  never asked).
- **Cache** in Redis `dns:mx:<domain>` with TTL; cache the negative (no-MX) result too, shorter TTL.
- Configurable resolver address (avoid the local stub); use `miekg/dns` if the stdlib resolver is
  insufficient for control over servers/TTL.

## Tasks

- [ ] MX/A resolution with priority sort + guard integration
- [ ] No-MX fallback + "no mail server" verdict path
- [ ] Redis cache (`dns:mx:<domain>`) with positive/negative TTL
- [ ] Configurable resolver; unit tests with a stub resolver
- [ ] Update `redis-contract.md` (dns key), `service/dns-resolver.md`, `operations/dns.md`, changelog

## Definition of Done

- [ ] `void`-type domain (no MX, no A) → "no mail server" `unknown`, not `invalid` — test
- [ ] `nomx`-type domain (A, no MX) → probes the A host — test
- [ ] Repeat lookup served from cache — test
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Keep the guard as the resolver's responsibility (002) — resolution and vetting are one step so a
caller cannot get an unvetted IP.
