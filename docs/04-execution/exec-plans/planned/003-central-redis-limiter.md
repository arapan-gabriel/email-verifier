# Plan 003 — central-redis-limiter

**Status:** Planned
**Phase:** A
**Depends on:** 001

## Goal

Make the shared Redis token bucket THE limiter for every probe, with per-MX AIMD bands, so pacing is
correct and node-count-agnostic — and fails closed when Redis is unreachable.

## Context

`001` probes at a fixed rate. The lab has the pacing model (`ratecheck/internal/verify` limiter) and
the central bucket (`config/limiter/token_bucket.lua`), plus the Redis contract. This plan wires them
in as the authority. Patterns: `aimd-pacing.md`; contract: `06-generated/redis-contract.md`;
decision: ADR-004.

## Design

- `internal/limiter` — load `token_bucket.lua`; take+refill in one round trip against
  `rt:mx:<host>:bucket`. One bucket per MX, shared across nodes (invariant 4).
- `internal/pacer` — per-MX AIMD over the bucket: start at `max_rate`; `×0.5` on `IsThrottle`;
  `×1.1` after 10 clean; pause at floor; move only inside `[min_rate,max_rate]`. **Only `IsThrottle`
  moves it** — deferrals and `ClassPolicy` are retried but never lower the rate or arm the pause
  (carry the `ds-smtp-retry` fix).
- Bands from `limits:mx:<host>` (seed from `config/limits/*.json`, ported from
  `ds-smtp-retry/config/limits-init`); unknown MX → conservative default.
- **Fail closed:** a Redis error on the pacing path → skip probe, return `unknown` (invariant 5).
- `internal/redis` — port the minimal RESP client (SET/GET/EVAL/SCAN/PING).
- Every probe now acquires a token before connecting; state keys (`rt:mx:*`) updated per the
  contract.

## Tasks

- [ ] Port `internal/redis` (RESP client, EVAL for Lua)
- [ ] `internal/limiter` around `token_bucket.lua` (take+refill, one round trip)
- [ ] `internal/pacer` AIMD; deferral/policy do NOT move it (unit-tested)
- [ ] Band loading from Redis + seed JSON; conservative default for unknown MX
- [ ] Fail-closed path → `unknown` on Redis error
- [ ] Wire pacer into `/verify` (acquire token before connect)
- [ ] Update `06-generated/redis-contract.md`, `service/limiter.md`, `changelog.md`

## Definition of Done

- [ ] Two concurrent `/verify` bursts to one MX stay under the band (integration test with `mxsim`)
- [ ] Deferral (`4.2.2`) and policy (`5.7.x`) do not lower the rate; a `421` does — unit tests
- [ ] AIMD recovery, the cooldown pause and bucket refill are tested inside `synctest.Test` on the
      fake clock, not by sleeping (ENGINEERING-STANDARDS §7)
- [ ] Redis down → `unknown`, no probe sent — test
- [ ] gates clean; pr-checklist central-bucket + fail-closed confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

The bucket being central here is what makes ADR-004 true — after this plan, adding a node is a
deploy, not an engine change. Do not introduce any per-process rate state.
