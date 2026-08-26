# Service — limiter (`internal/limiter`) & pacer (`internal/pacer`)

The central Redis token bucket and the per-MX AIMD loop over it.

- Bucket: `docs/07-references/token_bucket.lua`, take+refill in one round trip, shared across nodes
  (invariant 2b, ADR-004).
- Pacing model & what may move it: `docs/03-engineering/patterns/aimd-pacing.md`.
- Keys: `docs/06-generated/redis-contract.md`.
- Fail closed on Redis error (invariant 3).
