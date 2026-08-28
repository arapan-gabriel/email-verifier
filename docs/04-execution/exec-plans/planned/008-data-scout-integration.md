# Plan 008 — data-scout-integration

**Status:** Planned
**Phase:** A
**Depends on:** 001, 002, 003, 004, 005, 006, 007

## Goal

Cut Data Scout over to this service: `smtp_probe.probe_many` becomes an HTTP client to `POST /probe`
and port-25 traffic leaves the production host for good. Per ADR-006 the seam is the prober, not the
provider — everything above it (layers 0–5, domain grouping, scoring, signals, the domain-profile
cache, quota, suppression, the verdict table) is untouched.

## Context

This closes the loop the whole project is for. Data Scout keeps layers 0–5, quota, the
`email_verifications` table, and suppression; this service does 6–8 from the isolated IP. The change
lives in **both repos** — coordinate. Data Scout invariant 10 (provider timeout + per-domain cache)
must hold. Contract: `service/storage-contract.md`, `06-generated/api.md`.

## Design (Data Scout side — cross-repo)

- Replace the body of `app/core/verify/smtp_probe.probe_many` with an HTTP client to `POST /probe`.
  `set_prober` stays the seam, so every existing fake prober and test keeps working. The provider
  layer, `engine.verify_many`, the domain grouping (plan 067) and the timeout/cache contract (Data
  Scout invariant 10) are all unchanged.
- **A transport failure maps to `ProbeResult{connected: false}`** — never `accepted: false`.
  Invariant 1 governs this hop as it governs SMTP; a redeploy of the probe mid-request must not be
  able to condemn a mailbox.
- Map this service's `{status,...}` onto `email_verifications.status` using the reconciliation from
  005. Store `source_ip` in `signals` — a verdict is bound to the IP that produced it.
- Retire `app/core/verify/smtp_probe.py` and its in-process ceilings (superseded by this service's
  central bucket); keep its semantics as tests/reference.
- Bulk: Data Scout's verify job calls `/verify/bulk` and polls; results persisted as today.

## Design (this service side)

- Confirm auth (mTLS/API key) between the two hosts.
- Ensure the response carries everything Data Scout stores (`status, smtp_code, enhanced_code,
  catch_all, signals, source_ip, checked_at`).

## Tasks

- [ ] Data Scout: `smtp_probe.probe_many` → HTTP client over mTLS; `set_prober` seam preserved
- [ ] Data Scout: transport failure → `connected:false`, asserted by test
- [ ] Data Scout: status reconciliation + store `source_ip`
- [ ] Data Scout: retire `smtp_probe.py` in-process probing (keep as reference/tests)
- [ ] Data Scout: the Celery task keeps driving the chunk loop; it calls `/probe` once per domain
- [ ] **Data Scout owns the greylist retry** (plan 006). A `class:deferred` row is re-queued using
      the existing Celery backoff, scheduled by `retry_after_seconds` rather than by blind
      exponential backoff — the first blind attempt lands seconds later, before the window opens,
      and burns a token to be told the same thing. Exhausted retries are `unknown` with the server's
      own words, never `invalid`.
- [ ] **Data Scout keeps the retry on the same tuple.** Greylisting keys on
      `(sender, recipient, IP)`: a retry from a different node or with a different `MAIL FROM` is a
      new tuple and restarts the window. Automatic with one node; a routing constraint the moment
      there are two.
- [ ] This service: auth between hosts; response completeness
- [ ] Both: integration test of the end-to-end path
- [ ] Docs both repos: Data Scout providers doc + this `changelog.md`

## Definition of Done

- [ ] Data Scout's verify endpoint returns this service's verdict end-to-end (staging) — recorded
- [ ] Layers 0–5 still short-circuit before any call here (no wasted probes) — test
- [ ] `source_ip` stored with each verdict — test
- [ ] Both repos' gates green; pr-checklists confirmed
- [ ] Status → Complete, moved, ROADMAP updated; Data Scout changelog entry too

## Notes / decisions / deviations

Cross-repo plan — mirror an entry in Data Scout's `docs/08-decisions/changelog.md`. After this,
production has zero outbound `:25` from the main host (the reputation goal is met).
