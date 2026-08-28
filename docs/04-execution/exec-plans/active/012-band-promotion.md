# Plan 012 — calibration-as-a-service

**Status:** Active
**Phase:** B
**Depends on:** 003, 004

## Goal

Stop being stuck below a guessed ceiling — without deliberately provoking a provider from a
week-old IP.

## The gap, stated precisely

All 71 shipped bands carry `"confidence": "guess"`. AIMD moves only **within** `[min_rate, max_rate]`:
it halves on a throttle and climbs on clean answers, but `min(band.MaxRate, rate*1.1)` means it can
never discover that a provider tolerates more than the seed says. Gmail's seed is `1.0/s`. If Gmail
in fact tolerates five, this service will sit at one forever and nothing in it will ever notice.

That is the one thing the pacer cannot fix by itself, and it is what this plan is for.

## Context

`ds-smtp-retry/ratecheck/internal/calibrate` already implements ramp → bisect → soak → concurrency →
recovery to find an MX's knee and write a `[min,max]` band. Seed bands ship embedded in `internal/pacer/bands/*.json`.
This plan brings calibration into the service as a controlled operator action. Pattern:
`aimd-pacing.md`; contract: `redis-contract.md`.

## Why the active ladder is not what gets built

The plan's own note already preferred passive calibration. Three things have changed since, and all
three point the same way.

1. **The telemetry exists.** Plan 009 shipped `verify_rate_per_sec`, `verify_mx_state`,
   `verify_pause_events_total` and `verify_smtp_replies_total{code,class}` — the knee signal, from
   real work rather than from provoked failures.
2. **The IP is a week old with no sending history.** Deliberately ramping until Gmail answers `421`
   is how a fresh address gets listed. RUNBOOK Phase 5 puts laddering *after* warm-up, deliberately.
3. **The capability is not missing.** `ds-smtp-retry` is a working CLI with the full
   ramp/bisect/soak/concurrency/recovery ladder. An operator can run it from the node against a
   warmed IP. Porting its 615 lines plus the `report` package into this service would add an HTTP
   trigger, not a new ability — and a trigger for something its own plan says not to run yet.

So the ladder stays in the lab, and this plan builds the thing the pacer genuinely cannot do.

## Design — passive promotion

- The pacer counts consecutive clean answers **while sitting at the ceiling**. An MX that has
  answered cleanly `promote_after` times at its maximum has demonstrated that the maximum is not the
  limit — evidence gathered from work we were doing anyway.
- On that evidence it writes a **proposal** to `limits:mx:<host>:proposed`: a new ceiling, the
  evidence behind it, and when it was formed.
- **It never applies one.** Lowering is AIMD's business and is reversible; raising a ceiling is the
  one direction the loop cannot undo, and applying it automatically on a heuristic is how a service
  walks into a provider's limit. A proposal is promoted by an operator.
- `GET /admin/bands` lists what the pacer is tracking, with the settled rate and any proposal.
  `POST /admin/bands/promote {mx_host}` applies one to `limits:mx:<host>` and clears it. The pacer
  re-reads the band on its next miss, so a promotion bites without a restart.
- A proposal is **bounded** — a step, capped in absolute terms — so no run of clean answers can
  propose a rate nobody sanctioned.

## Tasks

- [ ] Port `internal/calibrate`
- [ ] `POST /admin/calibrate` (operator-auth) → writes `limits:mx:<host>`
- [ ] Live band re-read so new bands apply without restart
- [ ] Safety gates: healthy IP required, target validation, calibration rate-limit
- [ ] Tests against `mxsim` (rediscover a known limit — the lab's scenario 12)
- [ ] Ramp/bisect/soak timing is driven by `synctest`, so a soak window costs no wall-clock time
- [x] Update `api.md`, `redis-contract.md`, `aimd-pacing.md`, changelog

## Definition of Done

- [x] An MX answering cleanly at its ceiling produces a proposal, with the evidence recorded
- [x] **No ceiling is ever raised without an operator** — asserted
- [x] A single throttle resets the evidence and withdraws nothing already proposed
- [x] A proposal never exceeds the configured absolute ceiling
- [x] Promotion applies the band, clears the proposal, and takes effect without a restart
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated — pending manual sign-off

## Results (2026-08-28)

Gate clean. End to end on the node, against real Redis, with a `[1..4]/s` band:

```
GET /admin/bands
  rate 4, max 4, STEADY
  proposal: 4 -> 6, clean_answers_at_ceiling: 20

POST /admin/bands/promote {"mx_host":"92.222.87.97"}
  promoted: 4 -> 6

limits:mx:92.222.87.97   "max_rate_per_sec": 6
POST again               409 — the proposal is gone
GET without credentials  401
```

An earlier run with a `[10..50]/s` band produced **no** proposal, which is the absolute cap doing its
job: `min(20, 50×1.5)` is not above 50, so there was nothing to propose.

| Check | Result |
|---|---|
| Clean answers at the ceiling | proposal formed, with the evidence and when |
| Clean answers **below** the ceiling | no proposal — they say nothing about the ceiling |
| One real throttle | evidence reset |
| Step past the absolute ceiling | capped |
| Band already above the absolute ceiling | nothing proposed |
| Promotion | band widened on disk, proposal cleared, no restart |
| Promotion with nothing standing | `409` |
| `promote_after: 0` | no proposal after a thousand clean answers |

### A test that was wrong, and the code that was right

The first promotion test asserted the pacer would resume at the new ceiling. It resumes at the rate
it had **earned** instead, and climbs from there — because a saved rate may only ever lower the
start, and *a ceiling is earned by clean answers, never granted by config* (RUNBOOK). Promotion
widens the permission, not the rate. The assertion was corrected rather than the behaviour.

## Notes / decisions / deviations

For production, prefer passive calibration over actively provoking `421`s. That was this plan's own
note before it was implemented, and implementing it turned the preference into the whole design.

**The active ladder is deferred, not rejected.** When the IP has a sending history and a
provider's real limit matters commercially, `ds-smtp-retry`'s `calibrate` is the right tool and can
be run from the node — against one MX, deliberately, with someone watching. What would be wrong is
an HTTP endpoint that lets it happen by accident. If it is ever brought in-service, the guards the
original plan listed still apply: healthy IP, validated target, rate-limited runs.

**Promotion is deliberately manual.** A proposal is evidence, not a decision. AIMD can undo a rate
that turned out to be too high *within* a band; it cannot undo a band that was widened wrongly, and
the failure mode of a band that is too wide is a blocklisting rather than a slow run.
