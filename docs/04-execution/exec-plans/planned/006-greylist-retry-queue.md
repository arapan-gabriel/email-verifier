# Plan 006 — greylist-retry-queue

**Status:** Planned
**Phase:** A
**Depends on:** 003

## Goal

A persistent retry queue for greylisting and per-recipient deferrals that survives a process restart,
resolving addresses on their backoff without ever touching the shared pacer.

## Context

Greylisting is a `4xx` on first sight that clears on a later retry — per-recipient, rate-independent
(`patterns/retry-greylist.md`). It must be retried but must not move the pacer (that fix landed in
003). A retry due in 30 minutes cannot be lost to a redeploy, so the queue is persistent.

## Design

- A deferral (classed not-`IsThrottle`) schedules the address into a Redis-backed retry queue with a
  due time on a doubling backoff. Genuine rate `4xx` (`421`) stays on the AIMD path, not here.
- On startup, the service re-reads pending retries and their due times (persistence).
- After the retry budget is exhausted → `unknown` with the server's own words, **never `invalid`**.
- Per-MX pacing still applies to retries (they go through the bucket), so a greylisting server is
  never hammered.
- For the synchronous `/verify` path, a deferral returns promptly as `unknown` with a "retry
  scheduled" signal; the bulk path (007) surfaces the eventual resolved verdict.

## Tasks

- [ ] **Enable Redis persistence first** — `appendonly yes`, `appendfsync everysec`. Stock Debian
      runs RDB snapshots only and loses up to an hour on a crash, which makes this plan's central
      promise false. See `redis-contract.md`
- [ ] Redis-backed retry queue (address, due time, attempt count)
- [ ] Schedule on deferral; doubling backoff; budget cap → `unknown`
- [ ] Startup re-read of pending retries (survives restart) — test
- [ ] Ensure retries pass through the pacer/bucket (no bypass)
- [ ] Update `redis-contract.md` (queue keys), `patterns/retry-greylist.md`, changelog

## Definition of Done

- [ ] A greylisted (450 first, 250 later) address resolves via retry — integration test with `mxsim`
- [ ] The doubling backoff and the due-time arithmetic are tested under `synctest` — a 30-minute
      retry must be asserted in microseconds, never slept through
- [ ] Queue survives a simulated process restart — test
- [ ] Queue survives a Redis restart — verified against a Redis with AOF on
- [ ] Exhausted retries → `unknown`, never `invalid` — test
- [ ] Deferrals never moved the pacer during all this — assert
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Still no SQL DB — the queue is operational state in Redis.

The retry result has no synchronous caller to return to, and under ADR-006 there is no job here to
attach it to. Decide explicitly how the eventual verdict reaches Data Scout: the simplest option
consistent with "stateless about business data" is that the retry queue is **not** this service's
concern at all — a deferral comes back as `connected:true, accepted:null, class:deferred` and Data
Scout re-queues the address through machinery it already has. Resolve this before implementing.
