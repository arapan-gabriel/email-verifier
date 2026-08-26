# HTTP API (generated contract)

Kept in sync with `internal/api/*` in the same change. Authenticated (mTLS or API key) except health
probes. JSON in/out; error shape `{error:{code,message}}`.

> Greenfield — endpoints appear here as their plans land. Rows below are the planned contract.

## Health (unauthenticated)

| Method | Path | Plan | Response |
|---|---|---|---|
| GET | `/healthz` | 000 | `200 {"status":"ok"}` — liveness |
| GET | `/readyz` | 000 | `200` when Redis reachable; `503` otherwise |

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
