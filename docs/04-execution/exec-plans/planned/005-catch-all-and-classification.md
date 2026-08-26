# Plan 005 — catch-all-and-classification

**Status:** Planned
**Phase:** A
**Depends on:** 001

## Goal

Detect catch-all and randomising MXes so a meaningless `250` becomes `risky`, and reconcile the
internal four-verdict vocabulary with Data Scout's status set so the HTTP contract is exact.

## Context

`250` on a catch-all domain (or a Microsoft randomiser) proves nothing. The lab detects this with
extra bogus `RCPT`s (`-catch-all-probes`). Data Scout's `email_verifications.status` vocabulary is
`verified/valid/accept_all/risky/invalid/unknown` — the boundary must map onto it. Pattern:
`smtp-classification.md`; feature: `001-product/features/002-catch-all-detection.md`.

## Design

- Per-domain catch-all probe: N known-bad local parts at the domain (default 3; configurable, more
  for Microsoft). All accepted → catch-all → every `250` here is `risky`. Mixed → randomiser →
  `risky`. All rejected → real answers trusted.
- **Catch-all is a domain property; a randomiser is a server property.** A caught randomiser
  condemns every domain on that MX host; a genuine catch-all is per recipient domain and never
  projected onto neighbours (carry the lab's distinction).
- Cache the catch-all result per domain (Redis, TTL) so it is probed once, not per address.
- **Reconciliation table** (this plan's core doc): internal `{valid,invalid,risky,unknown}` →
  Data Scout `{valid, accept_all(→risky), risky, invalid, unknown}`. Document it in
  `service/storage-contract.md` and the feature doc; the boundary applies it.

## Tasks

- [ ] Catch-all/randomiser probe + per-domain vs per-server logic
- [ ] Cache catch-all verdict per domain (Redis)
- [ ] `catch_all` + `signals` in the `/verify` response
- [ ] Reconciliation table to Data Scout statuses; wire it at the boundary
- [ ] Tests: catch-all→risky, randomiser condemns host, real mailbox→valid
- [ ] Update `features/002`, `storage-contract.md`, `redis-contract.md`, changelog

## Definition of Done

- [ ] Catch-all domain → `risky`; normal domain real mailbox → `valid`; randomiser host → neighbours
      `risky` — tests
- [ ] Response `status` maps onto Data Scout's vocabulary (documented + tested at the boundary)
- [ ] gates clean; pr-checklist (250-on-catch-all → risky) confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Getting the per-domain vs per-server distinction wrong reports a nonexistent mailbox as `valid` — the
lab's measured failure mode. Keep 30 probes available for Microsoft-class randomisers.
