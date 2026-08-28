# Redis contract (generated)

The only durable state this service holds — operational, not business. Inherited from the
`ds-smtp-retry` contract; kept in sync with `internal/{limiter,pacer,iphealth}` in the same change.

> No SQL database exists by design (ARCHITECTURE §"State ownership"). If Redis is lost, only the
> re-learnable working point is gone.

## Keys

| Key | Written by | Meaning |
|---|---|---|
| `limits:mx:<mx_host>` | operator / calibration (012) | Calibrated band JSON: `min_rate_per_sec`, `max_rate_per_sec`, `min_concurrency`, `max_concurrency`, `burst`, `cooldown_seconds`, `pause_seconds` |
| `rt:mx:<mx_host>:rate` | pacer | rate the AIMD loop has settled on |
| `rt:mx:<mx_host>:conc` | pacer | concurrency it has settled on |
| `rt:mx:<mx_host>:bucket` | limiter | token bucket hash — see `docs/07-references/token_bucket.lua` |
| `rt:mx:<mx_host>:state` | pacer | `PROBING` / `STEADY` / `BACKOFF` / `PAUSED` |
| `rt:mx:<mx_host>:pause_until` | pacer | epoch seconds; only this MX pauses |
| `ip:health:<ip>` | iphealth (010) | blocklist status / burned flag for a sending node |
| `dns:mx:<domain>` | resolver (004) | cached MX/A with TTL |

## Rules (enforced by tests)

- **Take and refill in one round trip.** `token_bucket.lua` does both; two calls let concurrent
  workers (or nodes) double-spend. This is invariant 4.
- **The bucket is central.** N nodes share one `rt:mx:<host>:bucket`. Never a per-process bucket.
- **Fail closed.** A Redis error on the pacing path means skip the probe (`unknown`), never send
  unpaced (invariant 5).
- AIMD moves inside `[min_rate, max_rate]` only; a saved runtime rate may only ever *lower* the
  start, never raise the ceiling.
