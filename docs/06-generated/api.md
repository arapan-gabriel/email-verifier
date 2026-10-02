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
| POST | `/send` | 014 — built, **route absent unless `relay.enabled`** |
| POST | `/admin/ip-health/complaint` | 015 — one spam complaint, reported by the caller |

```jsonc
// request
{
  "mx_host":        "gmail-smtp-in.l.google.com",  // attacker-influenced: SSRF-guarded here
  "domain":         "gmail.com",
  "emails":         ["a@gmail.com", "b@gmail.com"],
  "need_catch_all": true,                          // caller owns the domain-profile cache
  "helo":           "mail.datascoutmail.com",      // optional, defaults from config
  "mail_from":      "verify@probe.datascoutmail.com",
  "policy_stop":    6                               // optional; clamped, see below  // isolated from the sending domain (019)
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
`tls_failed`, `guarded`, `no_budget`, `paused`, `unknown`. The last three are all our own refusals to send:

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

- `tls_failed` (plan 031) — the server advertised `STARTTLS`, the handshake failed, **and the one
  plaintext retry on a fresh connection could not reach the server either** (`err` names both). About
  the conversation, never about the mailbox, and never a throttle — it does not move the pacer.
  `connected:false`, `accepted:null`, a retry hint. A refused `STARTTLS` or a failed handshake whose
  plaintext retry answers is not `tls_failed`: the verdict stands, with `tls: fallback`.

**`tls`** (plan 031) is how the session behind a result was encrypted, additive and omitted when no
session was opened (our own refusals before the socket): `none` (the server did not offer `STARTTLS`,
or `probe.starttls: off`), `verified` (upgraded; the certificate chains to a trusted root and names
the MX), `unverified` (upgraded; the certificate was not trusted or not for this name — recorded,
never enforced, as an opportunistic MTA does), `fallback` (offered, but `STARTTLS` was refused or the
handshake failed; the answer came in plaintext — on the same connection after a refusal, on one fresh
connection after a failed handshake), `skipped` (the host is remembered as TLS-broken,
`mx:<host>:tls_broken`, so the session went plaintext without asking) or `failed` (the handshake and
the plaintext retry both failed — the class is `tls_failed`). Callers may ignore it; the classes they
map are unchanged.

**`retry_after_seconds`** is present only on classes that mean "come back later" — `deferred`,
`throttled`, `no_budget`, `paused`, `tls_failed`. For `paused` it is exact, because the pacer knows when the
cooldown ends; otherwise it is parsed from the server's reply when it offers a number and falls back
to `probe.deferral_retry`. It is always clamped, so a server does not get to set the caller's
schedule. An answered address (`valid`, `invalid`) carries no hint.

**`policy_stop` is the caller's to move, within a bound.** The caller knows the shape of its
question and this service cannot: a finder's candidate ladder is a list of *guesses*, and every
wrong guess at a Microsoft tenant answers `550 5.4.1 Access denied` — a reply about us, not about
the address. Measured live: with the default of 5 and a six-rung ladder, **the sixth candidate is
never asked**, at every M365 tenant, every time. A verification batch of real addresses has no such
shape and wants the default.

Omitted or `0` uses the configured default. The value is **clamped to `probe.policy_stop_max`,
never refused** — an over-ambitious number is not a malformed request, and a `400` would reach the
caller as a transport failure and turn the whole batch into non-answers. `1` is raised to `2`, for
the same reason the configuration refuses it: one `5.7.x` can be a per-recipient policy, and
stopping on it would throw away the batch on one server's opinion of one address.

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
own words and `err` the transport error, both for the audit trail. **`reply` is the whole reply**
(plan 022): every line of a multi-line answer, joined by `\n` with its own `NNN-` prefix, first line
first, kept to 4096 bytes. Before 022 it was the final line only — which for a multi-line refusal is
the sign-off, not the reason.

> **Reading `class` without `catch_all` is a contract violation, not a style preference**
> (invariant 7). `class` classifies the *reply*; `catch_all` says whether that reply could mean
> anything. A `250` from a domain that accepts everything is `class: "valid"` **and** worth nothing,
> and this service will never say `risky` — that word is the caller's scoring of the two fields
> together, and it belongs there because the caller owns the verdict table. A consumer that maps
> `class` alone will mark every address at every catch-all domain deliverable.

**`catch_all` and `randomiser` answer different questions.** `catch_all` is about the *domain*: it
takes anything, so no `250` from it means a thing. `randomiser` is about the *server*: it answers
inconsistently, so no `250` from it means a thing **for any domain it hosts**, including ones nobody
has asked about. A randomiser sets `catch_all: true` as well — the conservative reading, and the
field callers already handle correctly.

Both are established by known-bad local parts, **asked only when the session accepted a real
address** — the verdict qualifies a `250` and nothing else — and **one at a time, stopping at the
first rejection** (plan 029): a rejected first one settles *clean*; an accepted one earns the next,
up to `probe.catch_all_probes` (default 2): all accepted → catch-all, accepted then rejected →
randomiser. A session with no accepted real address leaves both fields `null` (not established —
ask again later), and the question moves to the first session in the request that has a `250`. An
audit sample (`probe.catch_all_audit_rate`, default 0.05) asks the full old sequence of 3 regardless,
so the randomiser rate the early stop could miss stays measured. The verdict for a server is
remembered, so a later request for a different domain on that host carries it without re-probing.

**Several domains in one request (plan 033).** Instead of `domain`/`emails`/`need_catch_all`, a
caller may send `domains` — several domains answered by the same `mx_host`:

```jsonc
{
  "mx_host": "aspmx.l.google.com",
  "domains": [
    {"domain": "a.example", "emails": ["x@a.example", "y@a.example"], "need_catch_all": true},
    {"domain": "b.example", "emails": ["z@b.example"],                "need_catch_all": false}
  ]
}
```

Rules: the two shapes are exclusive (`domains` with any of `domain`/`emails`/`need_catch_all` is a
`400`); every entry names a domain and has at least one address, every address is at its entry's
domain (case-insensitive); the total across entries is within `server.max_emails_per_request`.
`helo`, `mail_from` and `policy_stop` keep their meaning. An entry repeating a domain is merged,
and an address is asked once. **The response is unchanged**: `results` keyed by address, and each
result's `catch_all`/`randomiser` are its own domain's (a randomiser, being about the server, marks
every domain in the request).

Whether the domains share an SMTP session is **this service's decision, per receiving system**:
only a family with `multi_domain: true` in `internal/pacer/families.json` (`@google` today) is
asked about several domains in one session — whole domains packed up to
`probe.max_rcpt_per_session`, then each asked domain's catch-all question after the real `RCPT`s.
Any other host is served one session per domain: the same answers, leases and tokens as separate
requests. One lease per session and one token per `RCPT` either way. A policy-stop ends the session
for every domain in it. A receiver refusing a domain other than the session's first **as relay**
(`relay access denied`, `relaying denied`, `5.7.64`, `not permitted to relay`, …) gets that address
`class:policy` — never `invalid` — the rest of the session is asked again one domain per session,
and grouping is switched off for that family on this node until restart (counter
`verify_multi_domain_fallbacks_total`, log `multi_domain_fallback`). A relay refusal does not feed
IP health or the stand-down: it is about the question, not our reputation.

A caller must not send `domains` to a service older than plan 033 — `DisallowUnknownFields` makes it
a `400` — which is why Data Scout gates it behind `VERIFY_MULTI_DOMAIN_SESSIONS`.

A batch is split at `probe.max_rcpt_per_session` — an unbounded recipient list is itself a
harvesting signal, and servers commonly cap it near 100. Catch-all is probed once per request, not
once per chunk: it is a property of the domain — in the first chunk with a `250`.

`connected`, `accepted`, `catch_all` and `randomiser` are tri-state: `null` means the server never gave a usable
answer, which is a different fact from `false`. They map one-to-one onto Data Scout's existing
`smtp_probe.ProbeResult`.

`source_ip` is always present — a verdict is only as good as the IP that produced it, and Data Scout
stores it in `email_verifications.signals`.

**A transport failure is not a verdict.** A timeout, a 5xx or a reset from this service means
`connected=false` on the caller's side, never `invalid` (invariant 1, ADR-006).

> There is no bulk endpoint and no job here. Orchestration — chunking, progress, quota, the result
> artifact — belongs to Data Scout's Celery task (ADR-006).

## POST /send

Queue one transactional message for delivery from the isolated IP (plan 014). The route exists only
when `relay.enabled` is set — a node that only verifies should not be one DKIM key away from being
able to send.

```jsonc
// request
{
  "from":    "noreply@datascoutmail.com",   // must be at the domain the relay signs for
  "to":      "someone@example.com",
  "subject": "Reset your password",
  "text":    "Hello.\n",
  "html":    "<p>Hello.</p>",               // optional; both makes it multipart/alternative
  "headers": [{"name": "X-Entity-Ref", "value": "abc"}]   // optional
}
```
```jsonc
// 202
{"message_id": "Nr7k…@datascoutmail.com", "queued_at": "2026-09-11T12:00:00Z"}
```

**`202` means durable, not delivered.** The message is signed and on disk before this answers, so a
caller may stop holding it. Whether a receiving server accepts it is not known yet and does not come
back here — it arrives as a bounce (plan 015).

| status | meaning |
|---|---|
| `202` | queued and durable |
| `400` | a verdict about this message — a malformed address, a reserved header, a suppressed recipient. Retrying will not change it |
| `503` | this node is not sending *at all* right now: the IP is listed, or the suppression list is stale or unreadable. Not this message's fault, and retryable |

**Headers this service sets may not be supplied**: `From`, `To`, `Subject`, `Date`, `Message-ID`,
`MIME-Version`, `Content-Type`, `Content-Transfer-Encoding`, `DKIM-Signature`, `Return-Path`,
`Received`. A caller that could choose `From` could send as anyone this domain can sign for. A CR or
LF anywhere in a header value is refused rather than escaped.

The envelope sender is **not** `from`: it is a VERP address at `relay.return_path_domain`, so a
bounce identifies its message without the body being parsed.

## Relay (phase C)

| Method | Path | Plan | Request | Response |
|---|---|---|---|---|
| POST | `/send` | 014 | `{from, to, subject, ...}` | `{message_id, queued_at}` |

## Operator (internal)

| Method | Path | Plan | Purpose |
|---|---|---|---|
| GET | `/admin/ip-health` | 010 | whether this node has stood itself down, and why |
| POST | `/admin/ip-health/resume` | 010 | clear the pause without a redeploy — the next scheduled check re-evaluates, so this overrides a verdict rather than disabling checking |
| GET | `/admin/bands` | 012 | what the pacer has learned per MX, and any standing proposal to widen a band |
| POST | `/admin/bands/promote` | 012 | apply a proposal: `{mx_host}` |
| POST | `/admin/bands/resume` | 032 | lift a stand-down early: `{mx_host}` — a host or a family key (`@microsoft-eop`); clears the key's `pause_until` and its refusers. `200 {"resumed": …}`, `503` if Redis refuses |
| GET | `/admin/suppress` | 011 | size, version and staleness of the local suppression copy |
| POST | `/admin/suppress` | 011 | push an export: `{version, hashes[], mode}` where mode is `replace` or `add` |

### A band is widened by a person, never by the loop

AIMD moves only inside `[min_rate, max_rate]`, and every shipped band says `"confidence": "guess"`.
An MX answering cleanly at its ceiling for `pacer.promote_after` probes has shown the ceiling is not
the limit — so the pacer records a **proposal** at `limits:mx:<host>:proposed`, with the evidence,
and stops there.

It never applies one. Lowering a rate is reversible and belongs to the loop; raising a ceiling is
not, and the failure mode of a band that is too wide is a blocklisting rather than a slow run.

Promotion widens the *permission*, not the rate: the pacer resumes from the rate it had earned and
climbs from there. A ceiling is earned by clean answers, never granted by config.

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
