# Plan 006 — greylisting: make a deferral actionable

**Status:** Complete (2026-08-28)
**Phase:** A
**Depends on:** 001, 003

> Renamed from `006-greylist-retry-queue`, because **the queue is not built here.** The open question
> this plan carried — "a deferred retry has no synchronous caller to return to" — resolves against a
> queue on this side, for a reason that is not a preference: **a retry's answer has nowhere to land
> here.** This service is stateless about business data (ADR-003) and owns no jobs (ADR-006), so a
> retry it performed by itself would produce a verdict with no consumer.
>
> Data Scout is the only place the answer can go. It already has the machinery: Celery with
> exponential backoff (`email_tasks.py`, `email_max_retries`, `email_retry_backoff_seconds`), the
> job, the row, and the quota accounting. Retries also stay correctly paced there, because a retry
> is just another `POST /probe` and goes through the same bucket.

## Goal

Give the caller everything it needs to schedule the retry itself, and write down the one constraint
that makes a retry work at all.

## Context

Greylisting is a `4xx` on first sight of a `(sender, recipient, IP)` tuple that clears when the same
tuple comes back later. Today a deferral is reported as `class:deferred`, `accepted:null` with the
server's own words — correct, and enough to know *that* a retry is needed, but not *when*.

Blind exponential backoff retries too soon: the first attempt lands seconds later, when the
greylist window has not opened, and burns a token to be told the same thing. Most greylisters open
after five to fifteen minutes, and some say so in the reply.

## Design

- **`retry_after_seconds` in the result** whenever the class means "come back later" — `deferred`,
  `throttled`, `paused`.
  - `paused` is exact: the pacer knows when the cooldown ends, so `pacer.PausedError` carries it.
  - `deferred` and `throttled` use an explicit hint parsed from the reply when the server gives one,
    otherwise a configured default. Parsing prose is fragile, so the parse is narrow, and any value
    it produces is clamped to a sane range — a server claiming "retry in 3 seconds" or "in 30 days"
    does not get to set our schedule.
- **No queue, no callback, no result store.** Adding any of them would make this service stateful
  about who asked, which is the thing ADR-003 rules out.

### The constraint that has to be written down

Greylisting keys on `(sender, recipient, IP)`. A retry that arrives from a **different sending IP**,
or with a **different `MAIL FROM`**, is a new tuple and starts the window again — forever, if the
caller keeps rotating.

With one probe node this is automatic. With more than one it is a real trap, and it is a constraint
on the *caller*: a retry must be routed to the same node and sent with the same envelope sender.
That belongs in `patterns/retry-greylist.md`, in plan 008, and in whatever multi-node plan exists,
because nothing in the code can enforce it from here.

## Tasks

- [x] `pacer.PausedError` carrying the remaining cooldown
- [x] `retry_after_seconds` in `prober.Result` for `deferred`, `throttled` and `paused`
- [x] Narrow parse of an explicit server hint, clamped; configured default otherwise
- [x] Tests: exact pause hint, parsed hint, default, clamping, absent for verdict classes
- [x] Rewrite `patterns/retry-greylist.md` around the caller owning the queue, with the tuple constraint
- [x] Record the cross-repo decision in plan 008; update `api.md`, changelog

## Definition of Done

- [x] A greylisted address returns `class:deferred` with a usable `retry_after_seconds`
- [x] A paused MX returns the exact remaining cooldown
- [x] `valid` and `invalid` carry no retry hint — they are answers, not deferrals
- [x] An absurd server hint is clamped rather than honoured
- [x] A `450`-then-`250` sequence resolves when the caller retries — integration test with `mxsim`
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [x] Status → Complete, moved to `completed/`, ROADMAP row updated

## Results (2026-08-28)

Gate clean.

| Check | Result |
|---|---|
| `mxsim` yahoo profile, first sighting | `class:deferred`, `accepted:null`, `retry_after_seconds` set |
| Same tuple after the window (clock advanced 6m) | `accepted:true`, **no** retry hint |
| `try again in 5 minutes` | 300 |
| `Please retry in 120 seconds` | 120 |
| `come back in 2 hours` | 7200 |
| Greylisted with no number | the configured default |
| `try again in 3 seconds` | clamped up to 60 |
| `try again in 30 hours` | clamped down to 6h |
| `valid`, `invalid`, `policy`, `guarded`, `conn_error` | no hint |
| Paused MX with 7 minutes left | ~420, exact |

## Notes / decisions / deviations

The original plan promised a Redis-backed queue that survives a restart, and `redis-contract.md`
still requires `appendonly yes` on the strength of it. **That requirement stays**: the calibrated
bands and the settled rate are worth keeping across a crash even though the retry queue is gone, and
enabling AOF cost nothing.

`452 4.2.2` (over quota) is left classified as a deferral even though, like `550 5.2.2`, it implies
the mailbox exists. The enhanced code travels with the result, so Data Scout can score that
inference where the rest of the scoring lives; making the classifier assert it here would be a
change to the lab's measured behaviour on the strength of an argument rather than a measurement.
