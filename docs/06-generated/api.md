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

## Probe — **the integration contract** (plan 001, ADR-006)

One batch per recipient MX. Data Scout has already run layers 0–5, resolved the MX and grouped the
addresses; this endpoint asks one server about several mailboxes in one session.

| Method | Path | Plan |
|---|---|---|
| POST | `/probe` | 001 — **live** |

```jsonc
// request
{
  "mx_host":        "gmail-smtp-in.l.google.com",  // attacker-influenced: SSRF-guarded here
  "domain":         "gmail.com",
  "emails":         ["a@gmail.com", "b@gmail.com"],
  "need_catch_all": true,                          // caller owns the domain-profile cache
  "helo":           "mail.datascoutmail.com",      // optional, defaults from config
  "mail_from":      "verify@probe.datascoutmail.com"
}
```
```jsonc
// 200
{
  "source_ip":  "92.222.87.97",
  "checked_at": "2026-08-28T12:00:00Z",
  "results": {
    "a@gmail.com": {"connected": true, "accepted": true,  "catch_all": false,
                    "smtp_code": 250, "enhanced_code": "2.1.5", "class": "valid",
                    "reply": "250 2.1.5 OK"},
    "b@gmail.com": {"connected": true, "accepted": false, "catch_all": false,
                    "smtp_code": 550, "enhanced_code": "5.1.1", "class": "invalid",
                    "reply": "550 5.1.1 The email account that you tried to reach does not exist"}
  }
}
```

`class` is the prober's classification, and it is the field that carries who a failure was about:
`valid`, `invalid`, `deferred`, `throttled`, `timeout`, `conn_error`, `bad_sequence`, `policy`,
`guarded`, `unknown`. `guarded` is our own refusal to open the socket — the `mx_host` resolved only
to addresses the SSRF guard rejects (invariant 2). Only `valid` and `invalid` are statements about a mailbox. `reply` carries the server's
own words and `err` the transport error, both for the audit trail.

A batch is split at `probe.max_rcpt_per_session` — an unbounded recipient list is itself a
harvesting signal, and servers commonly cap it near 100. Catch-all is probed once per request, not
once per chunk: it is a property of the domain.

`connected`, `accepted` and `catch_all` are tri-state: `null` means the server never gave a usable
answer, which is a different fact from `false`. They map one-to-one onto Data Scout's existing
`smtp_probe.ProbeResult`.

`source_ip` is always present — a verdict is only as good as the IP that produced it, and Data Scout
stores it in `email_verifications.signals`.

**A transport failure is not a verdict.** A timeout, a 5xx or a reset from this service means
`connected=false` on the caller's side, never `invalid` (invariant 1, ADR-006).

> There is no bulk endpoint and no job here. Orchestration — chunking, progress, quota, the result
> artifact — belongs to Data Scout's Celery task (ADR-006).

## Relay (phase C)

| Method | Path | Plan | Request | Response |
|---|---|---|---|---|
| POST | `/send` | 014 | `{from, to, subject, ...}` | `{message_id, queued_at}` |

## Operator (internal)

| Method | Path | Plan | Purpose |
|---|---|---|---|
| POST | `/admin/calibrate` | 012 | re-run a ladder for one MX, update its band |
| GET | `/metrics` | 009 | Prometheus exposition |
