# Plan 007 — bulk-verify-and-queue

**Status:** Planned
**Phase:** A
**Depends on:** 003, 004, 005

> **Largely superseded by ADR-006.** There is no bulk endpoint and no job on this service: Data
> Scout's Celery task already owns chunking, progress, quota metering, the result artifact and a
> reaper for stalled jobs, and it calls `POST /probe` once per domain. Duplicating that here would
> mean two definitions of "done" and left the resume-after-restart question unanswered.
>
> **What survives from this plan** is one behaviour that belongs in the prober: **policy-stop** —
> after N consecutive `ClassPolicy` replies from one server, stop probing it and mark the remainder
> `connected:false` with "not attempted: <reason>". Fold it into plan 001 or keep this plan as a
> thin follow-up for that alone.

## Goal

Verify a whole list as a job — paced per MX, catch-all-probed once per domain, greylist-retried —
with a job id and pollable results, laying the seam for a shared-queue worker mode later.

## Context

Single `/verify` covers realtime. Bulk is the volume path and is a job, not a request (build-vs-buy:
"this is a job-queue product"). The lab's `verify` command is the reference: group by MX, AIMD,
per-MX pause, catch-all once per domain, retries. Contract: `06-generated/api.md`.

## Design

- `POST /verify/bulk {emails[], helo?, mail_from?}` → `{job_id}`; `GET /verify/bulk/{job_id}` →
  `{state, done, total, results_url?}`.
- Engine reuse: group addresses by resolved MX (004), pace each group via the central bucket (003),
  probe catch-all once per domain (005), retry deferrals (006), work domains concurrently.
- Job + per-address results stored as **operational** state in Redis with a TTL (still no SQL DB);
  Data Scout pulls results and persists them.
- **Queue-worker seam:** the engine is transport-agnostic (ENGINEERING-STANDARDS §2). Structure the
  bulk runner so a future shared-queue consumer (ADR-003, deferred) calls the same engine entrypoint
  as the HTTP job — no re-implementation.
- `-policy-stop`-style behaviour: after N consecutive policy blocks from one server, stop probing it
  (it refused the client) and mark the rest `unknown` with "not attempted: <reason>".

## Tasks

- [ ] Bulk job endpoints + job state in Redis (TTL)
- [ ] Group-by-MX runner reusing pacer/resolver/catch-all/retry
- [ ] Concurrent domains; per-MX pause isolates one server
- [ ] Policy-stop after N consecutive blocks; rest → `unknown` "not attempted"
- [ ] Transport-agnostic engine entrypoint (HTTP job + future queue call the same fn)
- [ ] Results retrieval; update `api.md`, `redis-contract.md`, changelog

## Definition of Done

- [ ] A 1k-address mixed-MX bulk job completes, paced per MX, results retrievable — integration test
- [ ] One MX pausing does not stall other domains — test
- [ ] Policy-stop trips and the remainder is `unknown` "not attempted" — test
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Shared-queue worker mode is deferred (ADR-003, tech-debt) — this plan only guarantees the engine seam
so adding it later is additive, not a rewrite.
