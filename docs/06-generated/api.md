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
    "a@gmail.com": {"connected": true, "accepted": true,  "catch_all": false, "randomiser": false,
                    "smtp_code": 250, "enhanced_code": "2.1.5", "class": "valid",
                    "reply": "250 2.1.5 OK"},
    "b@gmail.com": {"connected": true, "accepted": false, "catch_all": false, "randomiser": false,
                    "smtp_code": 550, "enhanced_code": "5.1.1", "class": "invalid",
                    "reply": "550 5.1.1 The email account that you tried to reach does not exist"}
  }
}
```

`class` is the prober's classification, and it is the field that carries who a failure was about:
`valid`, `invalid`, `deferred`, `throttled`, `timeout`, `conn_error`, `bad_sequence`, `policy`,
`guarded`, `no_budget`, `paused`, `unknown`. The last three are all our own refusals to send:

- `guarded` — the `mx_host` resolved only to addresses the SSRF guard rejects (invariant 2).
- `no_budget` — the shared token bucket could not be consulted, so the probe failed closed
  (invariant 5). This is an incident: the node's Redis is unreachable and `/readyz` is already 503.
- `paused` — this MX is standing down for its cooldown after being throttled at the floor of its
  band. Normal operation.
- `ip_burned` — **this node** is standing down: its sending address is listed somewhere that
  matters, so probing on would deepen the damage and produce nothing worth having.
- `suppressed` — somebody asked to be forgotten. Never probed, never mailed (invariant 9). The only
  class that is a statement about the *request* rather than about the network.

All three return `connected:false` and `accepted:null`.

**`retry_after_seconds`** is present only on classes that mean "come back later" — `deferred`,
`throttled`, `no_budget`, `paused`. For `paused` it is exact, because the pacer knows when the
cooldown ends; otherwise it is parsed from the server's reply when it offers a number and falls back
to `probe.deferral_retry`. It is always clamped, so a server does not get to set the caller's
schedule. An answered address (`valid`, `invalid`) carries no hint.

**Policy-stop.** After `probe.policy_stop` *consecutive* replies that are about our client rather
than about a recipient (`class:policy`), the session ends and the remaining addresses come back
`connected:false`, `class:policy`, with `err` beginning `not attempted:`. A server that refuses us
refuses us for the whole session, so continuing would spend a token per recipient on a known answer
while hammering a server that has just said no. The count is consecutive: one `5.7.x` among ordinary
answers is a per-recipient policy, not a refusal of the client.

**There is no retry queue here.** A retry this service performed would produce a verdict with
nowhere to go — the row, the job and the quota are Data Scout's. It already has Celery backoff; what
this endpoint owes it is a deferral it can schedule against. See
`docs/03-engineering/patterns/retry-greylist.md`, including the tuple constraint that makes a retry
work at all. Only `valid` and `invalid` are statements about a mailbox. `reply` carries the server's
own words and `err` the transport error, both for the audit trail.

**`catch_all` and `randomiser` answer different questions.** `catch_all` is about the *domain*: it
takes anything, so no `250` from it means a thing. `randomiser` is about the *server*: it answers
inconsistently, so no `250` from it means a thing **for any domain it hosts**, including ones nobody
has asked about. A randomiser sets `catch_all: true` as well — the conservative reading, and the
field callers already handle correctly.

Both are established by `probe.catch_all_probes` known-bad local parts: all accepted → catch-all,
all rejected → the real replies stand, anything in between → randomiser. The verdict for a server is
remembered, so a later request for a different domain on that host carries it without re-probing.

A batch is split at `probe.max_rcpt_per_session` — an unbounded recipient list is itself a
harvesting signal, and servers commonly cap it near 100. Catch-all is probed once per request, not
once per chunk: it is a property of the domain.

`connected`, `accepted`, `catch_all` and `randomiser` are tri-state: `null` means the server never gave a usable
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
| GET | `/admin/ip-health` | 010 | whether this node has stood itself down, and why |
| POST | `/admin/ip-health/resume` | 010 | clear the pause without a redeploy — the next scheduled check re-evaluates, so this overrides a verdict rather than disabling checking |
| GET | `/admin/suppress` | 011 | size, version and staleness of the local suppression copy |
| POST | `/admin/suppress` | 011 | push an export: `{version, hashes[], mode}` where mode is `replace` or `add` |

### Suppression is pushed as digests, never as addresses

`POST /admin/suppress` accepts **salted SHA-256 digests only** — an entry that is not one is refused
rather than stored. A suppression list is a list of email addresses, and copying one onto this host,
for a mechanism whose purpose is erasure, would create the liability it exists to discharge.

```
hash = sha256(salt + "\x00" + strings.ToLower(strings.TrimSpace(value)))
```

`value` is either a full address or a bare domain — the source model suppresses both, and a
suppressed domain covers every address on it. The salt is shared configuration; a mismatch is
silent, because every lookup simply misses.

`replace` is what makes a removal at the source propagate; `add` only grows the set.



| Method | Path | Plan | Purpose |
|---|---|---|---|
| POST | `/admin/calibrate` | 012 | re-run a ladder for one MX, update its band |
| GET | `/metrics` | 009 | Prometheus exposition |
