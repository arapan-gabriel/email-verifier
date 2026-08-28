# ADR-006 — The seam is `probe_many`, and orchestration stays in Data Scout

**Status:** Accepted (2026-08-28)
**Supersedes in part:** ADR-003 (transport shape and bulk handling; its "stateless about business
data" decision is unchanged and in fact carried further here)

## Context

ADR-003 fixed the integration as `POST /verify {email}` behind Data Scout's
`app/core/providers/email_verify.py`, with bulk as a job on this service (ROADMAP 007). That was
written before the Data Scout side was read closely. It is:

```
verify_tasks._email_verify              Celery job: chunks, quota, per-row metering, progress
  └ email_verification_service.run_provider_many(emails)
      └ engine.verify_many(emails)
          ├ layers 0–5 locally (syntax, disposable, role, webmail, MX) — most addresses die here
          ├ survivors grouped BY DOMAIN                                  (Data Scout plan 067)
          ├ need_catch_all decided from its own domain-profile cache
          └ smtp_probe.probe_many(mx_host, emails, need_catch_all)       the only socket
                → dict[email] = ProbeResult{connected, accepted, catch_all, blocked}
```

Two facts change the design:

1. **Data Scout already resolves MX and already groups addresses by domain.** Its plan 067 did this
   deliberately: one SMTP session per domain instead of one per address, because "500 addresses no
   longer mean 500 connections" and it "is the difference between looking like a mail client and
   looking like a harvester."
2. **Data Scout already has the job machinery** — a `Job` model with
   `pending→running→succeeded/failed`, `progress`, `result_key` in object storage,
   `GET /jobs/{id}/result`, per-row quota metering, and a reaper for stalled jobs.

Cutting at the provider (`verify(email)`) would issue one HTTP request per address, destroying the
grouping — this service would either open one session per address or re-group work Data Scout had
already grouped. Building a job endpoint here would duplicate machinery that exists, works, and owns
the quota and the artifact.

## Decision

**1. The seam is `smtp_probe.probe_many`, not the provider.** The HTTP contract is a batch scoped to
one recipient MX:

```
POST /probe  {mx_host, domain, emails[], need_catch_all}
  → {source_ip, checked_at, results: {email: {connected, accepted, catch_all,
                                              smtp_code, enhanced_code, class}}}
```

It maps one-to-one onto the existing `ProbeResult` plus the evidence the boundary already requires.
`smtp_probe`'s internals become an HTTP client; `set_prober` stays the seam, so every existing fake
keeps working.

**2. This service has no jobs and no bulk endpoint.** It is a synchronous batch executor. The chunk
loop, progress, quota, artifact and retry stay in Data Scout's Celery task, which already does them.

**3. A transport failure is `unknown`, never `invalid`.** Invariant 1 applies to the HTTP hop
between the two services exactly as it applies to SMTP: a timeout, a 5xx, or a connection reset from
this service means `connected=false`, never a verdict about a mailbox.

**4. Authentication is mTLS.** The host sits on a public IP with `:25` open and is scanned
continuously; mTLS ends the handshake before a request reaches the application. An API key remains
as a second factor inside the tunnel for operator routes (`/admin/calibrate`, plan 012).

## Why

- **It preserves the anti-harvest shape.** The grouping is the concurrency, and it is what keeps the
  traffic looking like a mail client. A per-address contract would undo a decision Data Scout made
  on purpose.
- **It is the smaller change.** Everything above the socket — layers 0–5, scoring, signals, the
  domain-profile cache, quota, suppression, the verdict table — is untouched. Only the bottom-most
  function changes implementation.
- **It removes a duplicated subsystem.** Two job systems means two definitions of "done", two
  progress counters, and two places to look when a bulk run stalls.
- **It removes a resume hole.** ROADMAP 007 stored job state in Redis but never said what resumes
  the in-process worker after a restart. With no jobs here, a redeploy costs at most one in-flight
  chunk, which Data Scout retries through machinery it already has.

## Consequences

- `docs/06-generated/api.md`: `POST /probe` replaces `POST /verify`; `/verify/bulk` and its poll
  endpoint are dropped.
- **Plan 007 shrinks to almost nothing** — the bulk path is Data Scout's Celery loop calling
  `POST /probe` per domain. What remains worth keeping from it is the policy-stop behaviour
  (after N consecutive policy blocks from one server, stop probing it), which belongs in the prober.
- **Plan 004 narrows.** MX *discovery* stays in Data Scout (layer 5). This service resolves the
  supplied `mx_host` to A/AAAA and applies the SSRF guard — `mx_host` is still attacker-influenced
  data (a domain owner chose it), so invariant 2 is untouched and just as necessary.
- Plan 008 becomes a change to `smtp_probe` internals plus the mTLS link, not a rewrite of the
  provider layer.
- Catch-all detection is requested by the caller via `need_catch_all`, because Data Scout owns the
  per-domain profile cache that knows whether the answer is already held. This service performs the
  detection; it does not decide when to.
- The queue vocabulary is now unambiguous: **verification jobs** live in Data Scout, the
  **greylist retry queue** and the **per-MX token bucket** live here.

## Alternatives rejected

- **Keep `POST /verify {email}`** (ADR-003) — simplest contract, but issues one request per address
  and discards the domain grouping that exists specifically to avoid looking like a harvester.
- **Keep the bulk job here** (ROADMAP 007) — gives this service autonomy, but duplicates a working
  job system, splits the quota story across two hosts, and leaves the resume-after-restart question
  unanswered.
- **Callback/webhook on completion** — unnecessary once the caller drives the loop synchronously;
  it would also make this service stateful about who asked, which ADR-003 rules out.
