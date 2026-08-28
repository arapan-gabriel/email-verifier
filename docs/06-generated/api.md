# HTTP API (generated contract)

Kept in sync with `internal/api/*` in the same change. Authenticated (mTLS or API key) except health
probes. JSON in/out; error shape `{error:{code,message}}`.

> Endpoints appear here as their plans land. Rows marked **live** are implemented; the rest are the
> planned contract.

## Health (unauthenticated) — **live** (plan 000)

The only two routes that are not authenticated, and they must stay that way (invariant 11).

| Method | Path | Response |
|---|---|---|
| GET | `/healthz` | `200 {"status":"ok"}` — liveness; the process is up and serving |
| GET | `/readyz` | `200 {"status":"ready"}` when the operational store is reachable; `503 {"status":"not ready","reason":"…"}` otherwise |

`/readyz` currently dials the configured Redis endpoint. Plan 003 upgrades it to a real `PING` over
the RESP client — a successful dial is the honest extent of what can be asserted before that.

Any unmatched route returns `404 {"error":{"code":"not_found","message":"no such endpoint"}}`.

## Verify

| Method | Path | Plan | Request | Response |
|---|---|---|---|---|
| POST | `/verify` | 001 | `{email, helo?, mail_from?}` | `{status, smtp_code, enhanced_code, catch_all, signals, source_ip, checked_at}` |
| POST | `/verify/bulk` | 007 | `{emails[], helo?, mail_from?}` | `{job_id}` |
| GET | `/verify/bulk/{job_id}` | 007 | — | `{job_id, state, done, total, results_url?}` |

`status ∈ {valid, invalid, risky, unknown}`. `source_ip` always present — the verdict is bound to
the IP that produced it.

## Relay (phase C)

| Method | Path | Plan | Request | Response |
|---|---|---|---|---|
| POST | `/send` | 014 | `{from, to, subject, ...}` | `{message_id, queued_at}` |

## Operator (internal)

| Method | Path | Plan | Purpose |
|---|---|---|---|
| POST | `/admin/calibrate` | 012 | re-run a ladder for one MX, update its band |
| GET | `/metrics` | 009 | Prometheus exposition |
