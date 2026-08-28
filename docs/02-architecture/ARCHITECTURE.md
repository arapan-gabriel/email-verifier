# Architecture

## What this is

A single Go service, `verifierd`, deployed on an **isolated sending IP**. It answers two questions
for the Data Scout platform, both of which require outbound port 25 and IP reputation:

1. **Does this mailbox exist?** — an SMTP `RCPT TO` probe, paced per recipient MX.
2. *(phase 2)* **Send this transactional message** — outbound relay from the same warmed IP.

It is deliberately **stateless about business data**: it takes an address, returns a verdict, and
never stores who asked or what the answer was. Data Scout owns the verdict store, the quota, and the
product. This service owns only *operational* state — per-MX rate bands and IP health — in its own
Redis.

## The two-checker split (why this service is thin)

Verification is nine checks (build-vs-buy §1). They split cleanly by whether they touch port 25:

| Layer | Checks | Where it runs |
|---|---|---|
| **0–5** syntax · role · disposable · webmail · MX/DNS | no socket, instant, free | **Data Scout** (Python, already exists) |
| **6–8** TCP `:25` · `RCPT TO` · catch-all | needs the isolated IP + pacing | **this service** |
| 9 accept-all resolution | proprietary data, not reproducible | nobody — out of scope |

Data Scout runs 0–5 first and only calls this service for an address that survives them. That keeps
the expensive, reputation-bound work behind one network hop and one clear boundary.

## System diagram

```
                         HTTPS (mTLS / API key)
  ┌───────────────────┐   POST /verify {email}      ┌──────────────────────────────────┐
  │  Data Scout API   │ ──────────────────────────► │  verifierd  (ISOLATED IP)         │
  │  (prod host,      │                             │                                   │
  │   Cloudflare)     │ ◄────────────────────────── │  ┌────────────────────────────┐  │
  │                   │   {status, code, ec,        │  │ api      JSON, auth, timeouts│  │
  │  • layers 0–5     │    catch_all, signals}      │  ├────────────────────────────┤  │
  │  • quota/metering │                             │  │ resolver MX/A + cache + SSRF │  │
  │  • email_verif    │                             │  ├────────────────────────────┤  │
  │    table (store)  │                             │  │ pacer    per-MX AIMD ─┐      │  │
  │  • suppression    │                             │  │ prober   RCPT + catch-all    │  │
  │                   │                             │  │ classify RFC 3463 → verdict  │  │
  │  email_verify.py  │                             │  │ relay    (phase 2) DKIM send │  │
  │  = HTTP client    │                             │  └───────────┬──────────────┘   │  │
  └───────────────────┘                             └──────────────┼──────────────────┘
                                                                   │
                                                   ┌───────────────▼───────────────┐
                                                   │ Redis (operational state only) │
                                                   │  limits:mx:*  bands            │
                                                   │  rt:mx:*      live rate/bucket  │
                                                   │  ip:health:*  reputation        │
                                                   └────────────────────────────────┘
```

## Codemap (target layout)

Greenfield; this is where things go as plans land. Ported wholesale from `ds-smtp-retry/ratecheck`
unless noted.

```
cmd/verifierd/          service entrypoint: config, wiring, graceful shutdown
internal/
  api/                  net/http JSON handlers: /verify, /verify/bulk, /healthz, /readyz; auth
  config/               the one config source (env + file); no hardcoded anything
  resolver/             MX/A lookup, cache, no-MX fallback, + SSRF guard (NEW — not in the lab)
  prober/               one RCPT session + catch-all probe + reply classification  ← from lab
  pacer/               per-MX AIMD over the CENTRAL Redis token bucket             ← from lab
  limiter/             the shared bucket (token_bucket.lua) as THE limiter          ← from lab contract
  suppress/             suppression-list check before any probe/send (NEW)
  iphealth/             blocklist self-monitoring, "burned IP" detection (NEW)
  relay/                (phase 2) outbound mail: DKIM sign, queue, bounce handling
  metrics/              Prometheus exposition                                       ← from lab (mxsim)
  redis/                minimal RESP client                                         ← from lab
config/
  limiter/token_bucket.lua   the central take+refill bucket                         ← from lab
  limits/*.json              seed bands per provider                                ← from ds-smtp-retry/config/limits-init
scripts/preflight.sh         cold-IP go/no-go check                                 ← from ds-smtp-retry
```

## Where is the thing that does X?

| Question | File |
|---|---|
| Decides `valid`/`invalid`/`risky`/`unknown` from a reply | `internal/prober` (classifier) |
| Decides "whose fault is this 5xx" (RFC 3463 subject) | `internal/prober` (classify) |
| Paces requests to one MX, backs off, resumes | `internal/pacer` over `internal/limiter` |
| Stops N nodes double-spending a server's budget | `internal/limiter` (central Redis bucket) |
| Refuses to connect to a private MX | `internal/resolver` (SSRF guard) |
| Skips a probe when Redis is down (fail closed) | `internal/pacer` |
| Detects catch-all / randomiser | `internal/prober` (extra RCPT to a bogus local part) |
| Checks the suppression list | `internal/suppress` |
| Notices the IP got blocklisted | `internal/iphealth` |
| Signs and sends a real email (phase 2) | `internal/relay` |

## State ownership

| State | Owner | Store |
|---|---|---|
| Verdict per address, history, quota | **Data Scout** | its Postgres (`email_verifications`) |
| Suppression list (source of truth) | **Data Scout** | its Postgres; this service reads a synced copy/endpoint |
| Per-MX calibrated bands | this service | Redis `limits:mx:*` (the `ds-smtp-retry` contract) |
| Live rate / bucket / pause per MX | this service | Redis `rt:mx:*` |
| IP health / blocklist status | this service | Redis `ip:health:*` |

This service has **no SQL database**. If it dies, nothing about a customer's data is lost — only the
learned working point, which is re-learned. That is the whole point of the split.

### Which Redis, and why not Data Scout's

"Central" in ADR-004 means **shared between probe nodes** — one bucket per MX that node 1, node 2
and node 3 all draw from. It does not mean "on the application host". The operational Redis lives
next to the prober, currently on a unix socket with `port 0` set, and ADR-006 did not change that:
it moved *job orchestration* to Data Scout, not operational state.

Putting the bucket in Data Scout's Redis was considered and rejected:

- **The rate limit protects the sending IP, so it has to be enforced where the socket opens.** A
  limit applied by the caller is advisory: an operator running plan 012's calibration, a retry loop,
  a second integration or a stray `curl` all bypass it, and unpaced traffic leaves the IP anyway.
- **Take+refill runs once per probe.** A unix socket is microseconds; a cross-host round trip is
  milliseconds, on the hot path, at volume.
- **Fail-closed would become coupling.** Invariant 5 says no Redis, no probe — so a network blip
  between the two hosts would stop all probing, re-coupling the very hosts this project exists to
  separate.
- **Redis is currently not on the network at all.** Sharing it across hosts means exposing a service
  with no meaningful default authentication.

Open for the multi-node plan: *where* the shared Redis physically sits once there is more than one
probe node — on the first node, on a small dedicated host, or managed. ADR-004 fixes that it is
shared, not where it lives.

## Invariants

The binding list lives in `CLAUDE.md` ("Hard invariants"). The architectural ones:

- **us ≠ address** — a rejection of the client is never a verdict about the mailbox.
- **no private MX** — SSRF guard on every resolved IP.
- **central bucket** — pacing is shared across nodes, always.
- **fail closed** — no Redis, no probe.
- **policy ≠ throttle** — a `5.7.x` about our IP never drives pacing or condemns a mailbox.
- **stateless about business data** — verdicts belong to Data Scout.
- **IPv4 only** — the probe dials `tcp4`, never `tcp`. A dual-stack host prefers IPv6, and the
  sending identity (PTR, FCrDNS, SPF) is published for the IPv4 address only. Connecting from the
  IPv6 address means no FCrDNS and no SPF, which large providers answer with a `5.7.x` on `RCPT` —
  every verdict silently becomes `unknown` and it reads like a bug in the classifier.

## Sender identity

Published for the sending IP and carried in every session. HELO must FCrDNS to the connecting
address; `MAIL FROM` need not, which is what allows the verify/relay split.

| | HELO | `MAIL FROM` |
|---|---|---|
| Verification (Phase A/B) | `mail.<mail-domain>` | `verify@probe.<mail-domain>` |
| Relay (Phase C) | `mail.<mail-domain>` | `noreply@<mail-domain>` |

The `probe.` sub-domain keeps probing reputation off the root that sends real mail to customers; it
carries its own SPF because a receiving MX checks SPF of the `MAIL FROM` domain against the
connecting IP during a `RCPT` probe. Concrete values for the deployed node: plan 013.

## Integration contract with Data Scout

Settled by **ADR-006** (which supersedes ADR-003's transport shape).

- Transport: HTTPS with **mTLS**. One endpoint: `POST /probe`, a batch scoped to one recipient MX.
- **The seam is `smtp_probe.probe_many`, not the provider.** Data Scout runs layers 0–5, resolves
  MX, groups survivors by domain (its plan 067) and calls this service once per domain. Cutting
  higher would issue one request per address and destroy that grouping — which exists specifically
  so the traffic looks like a mail client rather than a directory harvest.
- Request `{mx_host, domain, emails[], need_catch_all}` → response
  `{source_ip, checked_at, results{email → {connected, accepted, catch_all, smtp_code,
  enhanced_code, class}}}`. Shapes in `docs/06-generated/api.md`.
- `source_ip` always travels with the answer — a verdict is only as good as the IP that produced it,
  and Data Scout stores it in `email_verifications.signals`.
- **No jobs here.** Chunking, progress, quota metering and the result artifact stay in Data Scout's
  Celery task, which already owns them. This service is a synchronous batch executor.
- **A transport failure is `unknown`, never `invalid`** — invariant 1 governs the HTTP hop exactly
  as it governs SMTP.
- `mx_host` is caller-supplied but still attacker-influenced (a domain owner chose that MX record),
  so it is resolved and SSRF-guarded here regardless (invariant 2).

See `docs/02-architecture/service/storage-contract.md`.
