# Plan 012 — calibration-as-a-service

**Status:** Planned
**Phase:** B
**Depends on:** 003, 004

## Goal

Expose the lab's ladder/band calibration as an operator endpoint so an MX's rate band can be measured
and updated live, instead of shipping static seed bands forever.

## Context

`ds-smtp-retry/ratecheck/internal/calibrate` already implements ramp → bisect → soak → concurrency →
recovery to find an MX's knee and write a `[min,max]` band. Seed bands ship embedded in `internal/pacer/bands/*.json`.
This plan brings calibration into the service as a controlled operator action. Pattern:
`aimd-pacing.md`; contract: `redis-contract.md`.

## Design

- Port `internal/calibrate` (ramp/bisect/soak/concurrency/recovery) from the lab.
- `POST /admin/calibrate {mx_host|domain}` (operator-auth) runs a calibration against that MX and
  writes the resulting band to `limits:mx:<host>`. The live pacer re-reads bands (watch/ticker, like
  the lab's `-watch-limits`) so a new band bites immediately.
- **Safety:** calibration deliberately provokes `421`s — refuse any target that is not the intended
  MX; require the IP to pass IP-health (010) first; rate-limit calibration runs. A band from
  calibration may raise a ceiling (it is measured); a band from a config file may only raise the
  floor (unproven).
- Never calibrate against a real MX from an unhealthy IP — provoking throttles on a shaky IP is how
  reputation dies (RUNBOOK safety note).

## Tasks

- [ ] Port `internal/calibrate`
- [ ] `POST /admin/calibrate` (operator-auth) → writes `limits:mx:<host>`
- [ ] Live band re-read so new bands apply without restart
- [ ] Safety gates: healthy IP required, target validation, calibration rate-limit
- [ ] Tests against `mxsim` (rediscover a known limit — the lab's scenario 12)
- [ ] Ramp/bisect/soak timing is driven by `synctest`, so a soak window costs no wall-clock time
- [ ] Update `api.md`, `redis-contract.md`, `aimd-pacing.md`, changelog

## Definition of Done

- [ ] Operator calibrates one MX; the new band takes effect on the live pacer without restart —
      integration test with `mxsim`
- [ ] Calibration refuses to run from an unhealthy IP or against a wrong target — test
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

For production, prefer passive calibration (read the knee off live telemetry from 009) over actively
provoking `421`s — document the trade-off; active calibration is an operator tool, not automatic.
