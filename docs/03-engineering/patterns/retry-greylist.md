# Pattern — greylisting

Greylisting is a `4xx` on first sight of a `(sender, recipient, IP)` tuple that clears when the same
tuple comes back later. It is **per-recipient and rate-independent**, so it must be retried without
touching the shared pacer (`aimd-pacing.md`).

## The queue is the caller's

This service holds no retry queue. Not a scoping preference — **a retry it performed by itself would
produce a verdict with nowhere to go.** It is stateless about business data (ADR-003) and owns no
jobs (ADR-006); the row, the job and the quota all live in Data Scout, which is therefore the only
place an eventual answer can land.

Data Scout already has the machinery: Celery with exponential backoff
(`email_max_retries`, `email_retry_backoff_seconds`). Pacing is preserved for free, because a retry
is just another `POST /probe` and goes through the same token bucket.

What this service owes the caller is a deferral it can *schedule against*.

## What comes back

- A deferral is `class: deferred`, `connected: true`, `accepted: null`, with the server's own words
  in `reply` and its enhanced code in `enhanced_code`.
- **`retry_after_seconds`** says when to come back. It is parsed from the reply when the server
  offers a number, and is `probe.deferral_retry` (default 15 minutes) otherwise. Any value is
  clamped: a server claiming "retry in 3 seconds" would have the caller burn a token before the
  window opens, and one claiming "in 30 days" would have it abandon a live address.
- After the caller's retry budget is exhausted the address is `unknown` **with the server's own
  words — never `invalid`**. That decision is the caller's too; nothing here reports a mailbox
  missing because a server said "later".

Only classes that mean "come back later" carry the hint — `deferred`, `throttled`, `no_budget`,
`paused`. A `valid` or an `invalid` is an answer, and attaching a retry to it would invite the caller
to re-ask a question that has been answered. For `paused` the hint is **exact**: the pacer knows
when the cooldown ends.

## The constraint that makes a retry work

Greylisting keys on the **tuple**. A retry that arrives from a different sending IP, or with a
different `MAIL FROM`, is a new tuple and starts the window over — forever, if the caller keeps
rotating.

With one probe node this is automatic. With more than one it is a trap, and it is a constraint on
the **caller**, which nothing in this service can enforce:

> A retry must go to the same probe node, with the same envelope sender, as the attempt that was
> greylisted.

That is a reason to prefer sticky routing by recipient domain when a second node appears, and it
belongs in whatever plan adds one.

## Not throttling

A genuine rate `4xx` (`421`, Outlook `451 4.7.650`) is `IsThrottle` and *does* drive AIMD. A
greylisting `4xx` is not, and neither is `452 4.2.2` over-quota. The classifier
(`smtp-classification.md`) decides which is which, and the pacer is only ever handed the boolean —
see `aimd-pacing.md`.

## Bounds

Per-MX pacing bounds how often the same server is contacted, retries included, so a greylisting MX
is never hammered while waiting for its window to open.
