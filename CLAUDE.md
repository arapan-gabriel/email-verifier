# email-verifier

A standalone **Go** service that runs on an **isolated sending IP** and owns everything that touches
outbound port 25: SMTP mailbox verification (`RCPT TO` probing) and, in phase 2, outbound
transactional mail relay. It exists so that the reputation of the main product IP (the Data Scout
API host) is never spent on port-25 traffic — the one thing its own build-vs-buy analysis said must
live on separate machines.

It is the production successor to two things: the in-process Python probe in Data Scout
(`apps/api/app/core/verify/smtp_probe.py`, plan 065) and the Go calibration lab in `ds-smtp-retry`.
The lab's engine — per-MX AIMD pacing, calibrated rate bands, catch-all detection, RFC 3463 reply
classification, the central Redis token-bucket contract — is the core this service is built on.

**Relationship to Data Scout.** Data Scout keeps the cheap local layers (syntax, disposable, MX,
webmail, role — build-vs-buy §8 "Layer 0"), quota/metering, and the `email_verifications` table.
This service is called by Data Scout's `app/core/providers/email_verify.py`, which becomes a thin
HTTP client (timeout + per-domain cache — Data Scout invariant 10). **Data Scout stores the
verdicts; this service is stateless about business data** and owns only operational state (per-MX
bands, IP health) in its own Redis. See `docs/02-architecture/ARCHITECTURE.md`.

Stack: **Go 1.25** · net/smtp (hand-rolled state machine, from `ds-smtp-retry/ratecheck`) · Redis
(pacing + bands, the `ds-smtp-retry` contract) · Prometheus · net/http (JSON API) · miekg/dns
(resolver). No database of its own by design.

---

## Hard invariants — never break these

1. **A rejection of *us* is never a rejection of the address.** Connection refusals, TLS errors,
   timeouts, 4xx greylisting, and any 5xx issued before `MAIL FROM` succeeds return
   `status=unknown` (probe blocked), never `invalid`. Only a 5xx `5.1.x`/`5.5.x` on `RCPT` after a
   good `MAIL FROM` may mean "no such mailbox". Deleting an address because a server was busy or
   blocked us is the worst failure this system can produce.
2. **Never connect to a private or loopback address.** The MX host is attacker-influenced data (a
   domain owner can point MX at `127.0.0.1`), so every resolved IP passes an SSRF guard before a
   socket opens. *(This is the one place the `ds-smtp-retry` lab does the opposite on purpose — the
   guard is new code here.)*
3. **Connect over IPv4 only — dial `tcp4`, never `tcp`.** The sending identity (PTR, FCrDNS, SPF) is
   published for the IPv4 address alone, but a dual-stack host prefers IPv6 and would leave from an
   address carrying neither. Providers accept such a connection and reject at `RCPT` with a `5.7.x`,
   which this service classes as `ClassPolicy` — so every verdict silently becomes `unknown` while
   looking like a classifier defect. Measured on the deployed node, not hypothetical.
4. **The rate budget belongs to the recipient MX, and its token bucket is central.** All pacing
   goes through the shared Redis bucket (`ds-smtp-retry` contract, `config/limiter/token_bucket.lua`,
   take+refill in one round trip). N probe nodes with local buckets means N× the intended rate at
   Gmail — the bucket is the one thing that must stay shared as the service scales past one IP.
5. **Rate ceilings fail *closed*.** If Redis is unreachable, skip the probe (return `unknown`)
   rather than send unpaced. An unconfirmed verdict is recoverable; a blocklist entry is not.
6. **`ClassPolicy` (a `5.7.x`/`554 blocked` about our IP) is never counted as throttling** and
   never lowers a mailbox to `invalid`. Slowing down does not grow a PTR record or exit Spamhaus; if
   it counted, one blocked IP would calibrate every provider to zero.
7. **`250` is not `valid` on a catch-all or randomising MX** — it is `risky`. Catch-all is a
   per-domain property; a randomiser (Microsoft) is a per-server property that condemns every domain
   on that host.
8. **Never send `DATA` during verification.** The probe asks the question and disconnects; no message
   is transmitted. (Outbound relay in phase 2 is a separate, authenticated code path.)
9. **Honour the suppression list.** An address Data Scout has suppressed (GDPR erasure) is never
   probed or mailed.
10. Config from one place (`internal/config`) — no hardcoded secrets, IPs, or connection strings.
11. The HTTP API is an internet-facing edge: every request is authenticated (mTLS or API key). No
    unauthenticated endpoint except `GET /healthz` / `GET /readyz`.

> Numbered 1–11 with no gaps or letters. Every doc cites these numbers; if the list changes,
> renumber it here and fix the citations in the same change.

---

## Standard work cycle

Every piece of work — bug fix, improvement, or feature — follows this five-phase cycle. It mirrors
Data Scout's so both repos feel the same.

### Phase 1 — Orient (session start)
1. `docs/04-execution/ROADMAP.md` — the sequenced plan list; what's next and its manual-test gate.
2. `docs/04-execution/exec-plans/active/` — in-progress plans; `planned/` — the queue.
3. `docs/04-execution/tech-debt.md` — relevant open items.
4. `docs/00-meta/AGENTS.md` — invariants and orientation.

### Phase 2 — Plan
Read: `docs/02-architecture/ENGINEERING-STANDARDS.md`, `docs/02-architecture/ARCHITECTURE.md`,
`docs/01-product/index.md`, the relevant `docs/02-architecture/service/*.md` and
`docs/03-engineering/patterns/*.md`, and `docs/04-execution/exec-plans/templates/plan-template.md`.
Save the plan as `docs/04-execution/exec-plans/active/<NNN>-<name>.md` — fill every section,
including Definition of Done.

### Phase 3 — Execute
Read `ENGINEERING-STANDARDS.md` and the pattern docs for what you are building
(`smtp-classification`, `aimd-pacing`, `ssrf-guard`, `retry-greylist`). Tick off plan checkboxes as
you go; write tests alongside code; keep the plan's Status current.

### Phase 4 — Quality check
Must match `.github/workflows/ci.yml` exactly:
1. `go test -race -count=1 ./...`
2. `go vet ./...`
3. `gofmt -l .` (no output)
4. `golangci-lint run`
5. `docs/05-quality/checklists/pr-checklist.md` — every item (SSRF guard, fail-closed, and
   "us ≠ address" verdicts are mandatory).

### Phase 5 — Update docs
| What changed | Doc to update |
|---|---|
| New/changed HTTP endpoint | `docs/06-generated/api.md` |
| New/changed Redis key | `docs/06-generated/redis-contract.md` |
| New/changed metric | `docs/06-generated/metrics.md` |
| Every plan (always) | `docs/08-decisions/changelog.md` |
| New tech debt | `docs/04-execution/tech-debt.md` |
| New/changed feature | `docs/01-product/features/<NNN>-<name>.md` + `index.md` |
| New component / pattern | the relevant `docs/02-architecture/service/*.md` or `docs/03-engineering/patterns/*.md` |
| Architectural invariant changed | this file + `docs/02-architecture/ARCHITECTURE.md` |

Finally set the plan's `**Status**` to `Complete` with the sign-off date, move it from `active/` to
`completed/`, and update its row in `ROADMAP.md`.

---

## Dev commands

```bash
go build ./...
go test -race -count=1 ./...
go vet ./...
gofmt -l .
golangci-lint run

# run the service (config from env / internal/config)
go run ./cmd/verifierd -config config/verifierd.yaml

# smoke a single address against the running service
curl -s localhost:8080/verify -H 'authorization: Bearer $TOKEN' \
  -d '{"email":"john.smith@gmail.com"}' | jq
```

> **Before ANY real run against real MXes:** the sending IP must pass the preflight —
> `scripts/preflight.sh` (ported from `ds-smtp-retry`): open `:25`, rDNS + FCrDNS, SPF/DKIM/DMARC,
> not blocklisted. A run from a blocked or reverse-DNS-broken IP measures nothing. See
> `docs/07-references/RUNBOOK.md`.
