# Pattern — per-MX AIMD pacing over a central bucket

How the service paces itself to each recipient MX. Ported from `ds-smtp-retry/ratecheck/internal/
verify` + `config/limiter/token_bucket.lua`.

## The band

Each MX has a calibrated band `[min_rate, max_rate]` (+ concurrency band, cooldown, pause) in Redis
(`limits:mx:<host>`). Seed bands ship in `config/limits/*.json` (from `ds-smtp-retry/config/
limits-init`); calibration (plan 012) refines them.

## AIMD

- Start at `max_rate` (the ceiling).
- On a **real throttle** (`IsThrottle`: `421`/timeout/reset): `rate ×0.5`, concurrency −1, down to
  `min_rate`.
- After 10 clean answers: `rate ×1.1`, concurrency +1, up to `max_rate`.
- At `min_rate` and still failing → **pause** this MX for its cooldown; other MXes keep going.
- Never move outside `[min_rate, max_rate]`.

## What may and may not move the pacer

- **Only `IsThrottle()` moves it** — a genuine rate signal. Per-recipient deferrals (greylisting,
  `4.2.2` over-quota) and `ClassPolicy` (IP blocks) are retried but never lower the rate or arm the
  pause. Otherwise three full mailboxes or one blocked IP would drag the whole MX to a crawl. This is
  the fix carried from `ds-smtp-retry`.

## The bucket is central (invariant 2b, ADR-004)

Pacing goes through the shared Redis token bucket (`token_bucket.lua`), take+refill in one round
trip. All probe nodes share one bucket per MX; a per-process bucket would let N nodes send N× the
rate. This is what makes "add a node" a deployment change, not a pacing change.

## Fail closed (invariant 3)

If the bucket call errors (Redis down), the probe is **skipped** (`unknown`), never sent unpaced. An
unconfirmed verdict is recoverable; a blocklist entry is not.

## Saved working point

A runtime rate may be persisted (`rt:mx:<host>:rate`) so a job resumes where it left off — but a
saved rate may only ever **lower** the start, never raise the ceiling. Backing off is a measurement;
a quiet hour below the ceiling is not evidence the ceiling moved.
