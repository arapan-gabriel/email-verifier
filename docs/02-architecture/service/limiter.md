# Service — limiter (`internal/limiter`) & pacer (`internal/pacer`)

The central Redis token bucket and the per-MX AIMD loop over it.

- Bucket: `internal/limiter/token_bucket.lua`, **embedded in the binary** (ADR-005 — a limiter that
  fails because a file is missing would fail open in the worst place). Take+refill in one round
  trip, shared across nodes
  (invariant 4, ADR-004).
- Pacing model & what may move it: `docs/03-engineering/patterns/aimd-pacing.md`.
- Keys: `docs/06-generated/redis-contract.md`.
- Fail closed on Redis error (invariant 5).
