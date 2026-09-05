# Pattern — per-MX AIMD pacing over a central bucket

How the service paces itself to each recipient MX. Ported from `ds-smtp-retry/ratecheck/internal/
verify` + the lab's `config/limiter/token_bucket.lua`, now embedded at
`internal/limiter/token_bucket.lua`.

## The band

Each MX has a calibrated band `[min_rate, max_rate]` (+ concurrency band, cooldown, pause) in Redis
(`limits:mx:<host>`). Seed bands ship embedded in `internal/pacer/bands/*.json` (from `ds-smtp-retry/config/
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

## The bucket is central (invariant 4, ADR-004)

Pacing goes through the shared Redis token bucket (`token_bucket.lua`), take+refill in one round
trip. All probe nodes share one bucket per MX; a per-process bucket would let N nodes send N× the
rate. This is what makes "add a node" a deployment change, not a pacing change.

## Fail closed (invariant 5)

If the bucket call errors (Redis down), the probe is **skipped** (`unknown`), never sent unpaced. An
unconfirmed verdict is recoverable; a blocklist entry is not.

## What the pacer is allowed to be told

`internal/prober` calls `Pacer.Observe(ctx, mxHost, throttled bool)` — a bare bool, derived from
`Class.IsThrottle()`. The pacer never sees the Class at all, so a deferral or a policy block cannot
reach it even by mistake. The rule that used to be a comment is now the signature.

Budget is one token **per recipient**, not per session. Batching many RCPTs down one connection
(ADR-006) must not spend less budget than asking them one at a time would.

`Acquire` returning `ErrPaused` or a bucket failure means the probe is not sent: the addresses come
back `connected:false` with `class:paused` or `class:no_budget`. Plan 009 counts them apart — one is
normal operation, the other an incident.

## The one thing the loop cannot do (plan 012)

AIMD moves inside `[min_rate, max_rate]` and no further, and every shipped band carries
`"confidence": "guess"`. So a provider that tolerates more than its seed says will never be found
out: the loop climbs to the ceiling, sits there, and nothing notices.

The fix is evidence rather than a ladder. An MX that answers cleanly **at its ceiling** for
`pacer.promote_after` probes has demonstrated the ceiling is not the limit — measured from work we
were doing anyway, not from deliberately provoked `421`s. The pacer writes a bounded **proposal** and
stops.

**Promotion is a person's decision.** Lowering a rate is reversible and belongs to the loop; raising
a ceiling is not, and a band that is too wide fails as a blocklisting rather than as a slow run. One
real throttle withdraws the evidence.

Promoting widens the permission, not the rate: the pacer resumes from what it had earned and climbs
from there, because a ceiling is earned by clean answers and never granted by config.

The lab's active ladder — ramp, bisect, soak — stays in `ds-smtp-retry`, where an operator can point
it at one MX deliberately, from a warmed IP. It is the right tool and the wrong thing to put behind
an HTTP endpoint.

## Saved working point

A runtime rate may be persisted (`rt:mx:<host>:rate`) so a job resumes where it left off — but a
saved rate may only ever **lower** the start, never raise the ceiling. Backing off is a measurement;
a quiet hour below the ceiling is not evidence the ceiling moved.
