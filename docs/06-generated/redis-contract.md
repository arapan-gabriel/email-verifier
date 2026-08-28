# Redis contract (generated)

The only durable state this service holds — operational, not business. Inherited from the
`ds-smtp-retry` contract; kept in sync with `internal/{limiter,pacer,iphealth}` in the same change.

> No SQL database exists by design (ARCHITECTURE §"State ownership"). If Redis is lost, only the
> re-learnable working point is gone — **with one exception, which is why persistence is not
> optional.**

## Persistence is a requirement, not a default

`appendonly yes` + `appendfsync everysec` must be set before plan 006 ships. Debian's stock Redis
runs RDB snapshots only (`save 3600 1 300 100 60 10000`), which loses up to an hour of writes on a
crash, an OOM kill or a power loss. A graceful restart saves, so a redeploy is safe either way.

For the calibrated bands and the settled rate that hour is tolerable — they are re-learned by
design. For the **greylist retry queue** it is not: plan 006 promises that a retry due in thirty
minutes survives a restart, and losing it means an address is never re-asked and Data Scout waits
forever for a verdict that will not come.

## Keys

| Key | Written by | Meaning |
|---|---|---|
| `limits:mx:<mx_host>` | operator / calibration (012) | Calibrated band JSON: `min_rate_per_sec`, `max_rate_per_sec`, `min_concurrency`, `max_concurrency`, `burst`, `cooldown_seconds`, `pause_seconds` |
| `rt:mx:<mx_host>:rate` | pacer | rate the AIMD loop has settled on |
| `rt:mx:<mx_host>:conc` | pacer | concurrency it has settled on |
| `rt:mx:<mx_host>:bucket` | limiter | token bucket hash — `internal/limiter/token_bucket.lua`, embedded |
| `rt:mx:<mx_host>:state` | pacer | `PROBING` / `STEADY` / `BACKOFF` / `PAUSED` |
| `rt:mx:<mx_host>:pause_until` | pacer | epoch seconds; only this MX pauses |
| `mx:<mx_host>:randomiser` | prober (005) | `1` with a TTL: this host answers inconsistently, so no `250` from it is trustworthy for **any** domain it serves |
| `ip:health:<ip>` | iphealth (010) | blocklist status / burned flag for a sending node |

## Not in Redis, on purpose

**DNS answers.** This contract reserved `dns:mx:<domain>`; plan 004 dropped it. A DNS answer is not
state that has to be shared between nodes, and the deployed node already runs a caching resolver —
so a Redis round trip to avoid a lookup the OS has cached would make the hot path slower, not
faster. Resolution is cached in process instead, and only *vetted* results are stored, because
caching before the SSRF guard would be a way to smuggle a refused address back in.

Redis holds what genuinely must be shared: the rate budget, the calibrated bands, IP health.

## Rules (enforced by tests)

- **Take and refill in one round trip.** `token_bucket.lua` does both; two calls let concurrent
  workers (or nodes) double-spend. This is invariant 4.
- **The bucket is central.** N probe nodes share one `rt:mx:<host>:bucket`. Never a per-process
  bucket — and "central" means shared between probe nodes, not hosted on the application host. This
  Redis belongs to the prober; see ARCHITECTURE §"Which Redis, and why not Data Scout's".
- **Fail closed.** A Redis error on the pacing path means skip the probe (`unknown`), never send
  unpaced (invariant 5).
- AIMD moves inside `[min_rate, max_rate]` only; a saved runtime rate may only ever *lower* the
  start, never raise the ceiling.
