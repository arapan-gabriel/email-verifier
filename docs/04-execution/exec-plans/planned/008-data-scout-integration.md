# Plan 008 — data-scout-integration

**Status:** Planned
**Phase:** A
**Depends on:** 001, 002, 003, 004, 005, 006, 007

## Goal

Cut Data Scout over to this service: `email_verify.py` becomes a thin HTTP client to `/verify`, the
in-process `smtp_probe.py` is retired, and port-25 traffic leaves the production host for good.

## Context

This closes the loop the whole project is for. Data Scout keeps layers 0–5, quota, the
`email_verifications` table, and suppression; this service does 6–8 from the isolated IP. The change
lives in **both repos** — coordinate. Data Scout invariant 10 (provider timeout + per-domain cache)
must hold. Contract: `service/storage-contract.md`, `06-generated/api.md`.

## Design (Data Scout side — cross-repo)

- Replace the in-process prober behind `app/core/providers/email_verify.py` with an HTTP client to
  `POST /verify`, keeping the existing provider interface, timeout, and per-domain cache untouched
  (invariant 10) — layers 0–5 still run first in Data Scout; only survivors are sent here.
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

- [ ] Data Scout: `email_verify.py` → HTTP client; keep provider interface + timeout + cache
- [ ] Data Scout: status reconciliation + store `source_ip`
- [ ] Data Scout: retire `smtp_probe.py` in-process probing (keep as reference/tests)
- [ ] Data Scout: verify job → `/verify/bulk`
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
