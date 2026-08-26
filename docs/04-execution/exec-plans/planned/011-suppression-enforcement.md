# Plan 011 — suppression-enforcement

**Status:** Planned
**Phase:** B
**Depends on:** 001

## Goal

Never probe or mail an address Data Scout has suppressed (GDPR erasure / opt-out). The suppression
list is honoured before any socket opens.

## Context

Data Scout owns suppression (its `suppressions`, feature 005 / plan 040). Invariant 7: an address
Data Scout has suppressed is never probed or mailed. This service reads a synced copy so it can
enforce locally without a round trip per address.

## Design

- `internal/suppress` — a checked set, synced from Data Scout (endpoint or periodic export), cached
  in Redis with a version/TTL. Source of truth stays in Data Scout.
- The check runs **before** resolution/probe (and before any relay send in Phase C). A suppressed
  address returns a `suppressed` outcome with an auditable reason — never probed, never mailed.
- Fail-safe: if the suppression sync is stale beyond a threshold, log loudly; policy choice
  (fail-open vs fail-closed on suppression) is documented here — default **fail-closed for send**
  (do not mail on stale suppression) and fail-open-with-warning for verify (a probe is lower risk
  than a send, but still logged).

## Tasks

- [ ] `internal/suppress` — sync from Data Scout + Redis-cached set with version/TTL
- [ ] Enforce before probe and before send; `suppressed` outcome + reason
- [ ] Staleness handling + the documented fail policy
- [ ] Tests: suppressed address skipped with reason; stale-sync behaviour
- [ ] Update `service/storage-contract.md`, `redis-contract.md`, `SECURITY.md`, changelog

## Definition of Done

- [ ] A suppressed address is skipped (not probed/mailed) with an auditable reason — test
- [ ] Stale sync follows the documented fail policy — test
- [ ] gates clean; pr-checklist (suppression) confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Source of truth is Data Scout; this is enforcement, not ownership. Keep the sync one-directional.
