# Plan 003 — central-redis-limiter

**Status:** Active
**Phase:** A
**Depends on:** 001

> **Which Redis.** The prober's own, on the local unix socket — not Data Scout's. "Central" in
> ADR-004 means shared between probe nodes, not hosted on the application host; the rate limit
> protects the sending IP and therefore has to be enforced where the socket opens, or any other
> caller bypasses it. Reasoning: ARCHITECTURE §"Which Redis, and why not Data Scout's".

## Goal

Make the shared Redis token bucket THE limiter for every probe, with per-MX AIMD bands, so pacing is
correct and node-count-agnostic — and fails closed when Redis is unreachable.

## Context

`001` probes at a fixed rate. The lab has the pacing model (`ratecheck/internal/verify` limiter) and
the central bucket (now embedded at `internal/limiter/token_bucket.lua`), plus the Redis contract. This plan wires them
in as the authority. Patterns: `aimd-pacing.md`; contract: `06-generated/redis-contract.md`;
decision: ADR-004.

## Design

- `internal/limiter` — load `token_bucket.lua`; take+refill in one round trip against
  `rt:mx:<host>:bucket`. One bucket per MX, shared across nodes (invariant 4).
- `internal/pacer` — per-MX AIMD over the bucket: start at `max_rate`; `×0.5` on `IsThrottle`;
  `×1.1` after 10 clean; pause at floor; move only inside `[min_rate,max_rate]`. **Only `IsThrottle`
  moves it** — deferrals and `ClassPolicy` are retried but never lower the rate or arm the pause
  (carry the `ds-smtp-retry` fix).
- Bands from `limits:mx:<host>` (seed embedded in `internal/pacer/bands/*.json`, ported from
  `ds-smtp-retry/config/limits-init`); unknown MX → conservative default.
- **Fail closed:** a Redis error on the pacing path → skip probe, return `unknown` (invariant 5).
- `internal/redis` — port the minimal RESP client (SET/GET/EVAL/SCAN/PING).
- Every probe now acquires a token before connecting; state keys (`rt:mx:*`) updated per the
  contract.

## Tasks

- [x] Port `internal/redis` (RESP client, EVAL for Lua)
- [x] `internal/limiter` around `token_bucket.lua` (take+refill, one round trip)
- [x] `internal/pacer` AIMD; deferral/policy do NOT move it (unit-tested)
- [x] Band loading from Redis + seed JSON; conservative default for unknown MX
- [x] Fail-closed path → `unknown` on Redis error
- [x] Wire pacer into `/verify` (acquire token before connect)
- [x] Update `06-generated/redis-contract.md`, `service/limiter.md`, `changelog.md`

## Definition of Done

- [x] Two concurrent `/verify` bursts to one MX stay under the band (integration test with `mxsim`)
- [x] Deferral (`4.2.2`) and policy (`5.7.x`) do not lower the rate; a `421` does — unit tests
- [x] AIMD recovery, the cooldown pause and bucket refill are tested inside `synctest.Test` on the
      fake clock, not by sleeping (ENGINEERING-STANDARDS §7)
- [x] Redis down → `unknown`, no probe sent — test
- [x] gates clean; pr-checklist central-bucket + fail-closed confirmed
- [ ] Status → Complete, moved, ROADMAP updated — pending manual sign-off

## Results (2026-08-28)

Gate clean. Unit and `synctest` suites cover the AIMD arithmetic; the run below is from the deployed
node against a real Redis, with `mxsim` bound to the public IP so the SSRF guard sees a routable
address rather than loopback.

**Pacing holds the band.** Band set to `[0.5..2]/s`, burst 1, then a 12-recipient batch:

```
12 адресов за 6.0 c = 1.99/с   потолок 2/с      ✅
rt:mx:...:rate  0.5    state  BACKOFF    conc  1
```

1.99/s against a 2/s ceiling. The rate then sat at the floor in `BACKOFF` because `mxsim` throttled
during the burst — AIMD halved 2 → 1 → 0.5 and stopped there, exactly as the band says.

**Fail closed, verified by stopping Redis** (invariant 5):

| | |
|---|---|
| `GET /readyz` | `503` with the reason |
| `POST /probe` | every address `class:no_budget`, `connected:false`, `accepted:null` |
| Connections to the MX | **zero** — `mxsim`'s log stayed empty |
| Redis restarted | recovered to `class:valid` with no restart of the service |

**Found by accident, worth keeping.** The first run had `verifierd` started as the wrong user, so
the Redis socket (`660 redis:redis`) was permission-denied. Every probe came back `no_budget` with
the reason in `err`, and not one socket was opened to the MX. A misconfiguration that could have
meant unpaced sending instead produced a clean, safe refusal.

`appendonly yes` + `appendfsync everysec` enabled on the node while here — plan 006 needs it and
`redis-contract.md` requires it.

## Notes / decisions / deviations

The bucket being central here is what makes ADR-004 true — after this plan, adding a node is a
deploy, not an engine change. Do not introduce any per-process rate state.

**Decisions taken while building it:**

- **`Observe(ctx, mxHost, throttled bool)`.** The pacer is never handed a `Class`. Invariant 6 used
  to be a comment asking people to remember that deferrals and policy blocks must not move the
  pacer; it is now the signature, and the prober derives the bool from `Class.IsThrottle()`.
- **One token per recipient, not per session.** Batching under ADR-006 must not spend less budget
  than asking one at a time would.
- **The Lua script and the seed bands are embedded** (`go:embed`). The release artifact is one file
  (ADR-005), and a limiter that fails because a file is missing would fail open in the worst place.
  Startup logs the band count so a packaging mistake is visible.
- **The RESP client was not ported wholesale.** The lab's dials a fresh TCP connection per command,
  which is right for a calibration CLI and wrong here — take+refill runs once per probe. The wire
  codec is ported; pooling, unix sockets, `context`, and `EVALSHA`-with-fallback are new.
- **Two new classes**, `no_budget` and `paused`, kept apart because plan 009 must alert on one and
  not the other.
- **Known bound, not a defect.** The bucket is shared, so N nodes cannot double-spend a server's
  budget — that is what ADR-004 guarantees. The *rate* each node passes to the script is its own
  in-memory value, persisted to `rt:mx:<host>:rate`, so two nodes converge rather than agreeing
  instantly. Revisit in the multi-node plan, alongside where the shared Redis physically lives.
