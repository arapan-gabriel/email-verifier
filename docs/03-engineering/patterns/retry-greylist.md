# Pattern — greylisting & retry queue

Greylisting is a `4xx` on first sight of a `(sender, recipient, IP)` tuple that clears on a later
retry. It is **per-recipient and rate-independent**, so it must be retried without touching the
shared pacer (`aimd-pacing.md`).

## Behaviour

- A `4xx`/timeout classed as a per-recipient deferral (not `IsThrottle`) schedules the address for
  retry on a backoff, and is **not** counted as a throttle and does **not** move the pacer.
- Retries use a doubling backoff. After the retry budget is exhausted the address is `unknown` with
  the server's own words — **never `invalid`**.
- A genuine rate `4xx` (`421`, Outlook `451 4.7.650`) is `IsThrottle` and *does* drive AIMD — the
  classifier (`smtp-classification.md`) decides which is which.

## Persistence (plan 006)

The retry queue must **survive a process restart** — a greylist retry due in 30 minutes cannot be
lost to a redeploy. Queue state lives in Redis (operational state; still no SQL DB). On restart the
service re-reads pending retries and their due times.

## Bounds

Retry counts and per-MX pacing bound how often the same server is contacted, so a greylisting MX is
never hammered while waiting for its window to open.
