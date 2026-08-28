# Roadmap

Sequenced execution plans for `email-verifier`. Each plan has a **manual-test gate** that must pass
before the next starts. Plans live in `exec-plans/{active,planned,completed}/`; this file is the
index and running order.

Architecture is locked by the ADRs (`docs/02-architecture/decisions/`):
Go on the `ds-smtp-retry` engine · scope = probe + relay (phased) · stateless about business data ·
systemd on the host, no container runtime · the seam is `probe_many`, orchestration stays in Data
Scout (ADR-006).

## Phase A — Verification service (the reason this repo exists)

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 000 | ~~scaffold-and-standards~~ **done 2026-08-28** | repo layout, `cmd/verifierd`, config, CI (test/vet/fmt/lint), static build + systemd unit, healthz, `mxsim` | ✅ gate green; `/healthz` 200; drains on SIGTERM |
| 001 | ~~http-verify-service~~ **done 2026-08-28** | lab prober ported; `POST /probe` batch-per-MX (ADR-006); auth; mTLS wired, enabled by 013 | ✅ correct per-address results in one session, against `mxsim` and against real MXes from the node |
| 002 | ssrf-guard-and-safety | resolver SSRF guard (no private/loopback MX); "us ≠ address" verdict rules enforced | probe of a domain whose MX points at `127.0.0.1` is refused, not attempted |
| 003 | central-redis-limiter | make the shared token bucket THE limiter; per-MX AIMD; fail-closed on Redis down | two concurrent `/verify` bursts to one MX stay under the band; Redis down → `unknown`, no send |
| 004 | dns-resolver-and-cache | A/AAAA of the supplied `mx_host` + SSRF guard + cache (narrowed by ADR-006 — MX discovery stays in Data Scout) | guarded host refused; cache hit on repeat |
| 005 | catch-all-and-classification | catch-all + randomiser detection; verdict vocabulary reconciled with Data Scout statuses | catch-all domain → `risky`; a real mailbox on a normal domain → `valid` |
| 006 | greylist-retry-queue | persistent retry queue for 4xx/greylisting that survives restart | greylisted address is retried later and resolves; queue survives a process restart |
| 007 | ~~bulk-verify-and-queue~~ **superseded by ADR-006** | only policy-stop survives (N consecutive policy blocks → stop probing that server); orchestration is Data Scout's Celery | policy-stop trips and the remainder is "not attempted" |
| 008 | data-scout-integration | Data Scout `email_verify.py` → HTTP client to this service; retire in-process `smtp_probe` | Data Scout verify endpoint returns this service's verdict end-to-end |

## Phase B — Operations & hardening

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 009 | observability | structured logs, Prometheus metrics, request tracing | metrics scrape shows per-MX rate, verdict counts, pause events |
| 010 | ip-health-and-blocklists | blocklist self-monitoring; "burned IP" detection + alert | a simulated listing flips IP health and pauses sends |
| 011 | suppression-enforcement | suppression-list sync from Data Scout; never probe/mail a suppressed address | a suppressed address is skipped with an auditable reason |
| 012 | calibration-as-a-service | expose ladder/band calibration (from the lab) as an operator endpoint | operator can re-calibrate one MX and the new band takes effect live |
| 013 | deployment | OVH/Hetzner host, rDNS/FCrDNS/SPF/DKIM, secrets, systemd + distro Redis, preflight gate | `scripts/preflight.sh` returns GO on the real host; service reachable over mTLS |

## Phase C — Outbound relay (send mail from the isolated IP)

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 014 | relay-outbound-mail | `POST /send`; DKIM signing; send queue; per-MX pacing reuse | a signed test message is delivered and passes SPF+DKIM+DMARC at mail-tester |
| 015 | relay-bounce-handling | bounce/complaint capture; feed IP health; suppression on hard bounce | a hard bounce marks the address and updates suppression |

## Notes

- Phases are ordered by dependency, not just priority. 001 needs 000; 003 needs 001; 008 needs
  003–005. Phase C (relay) is deliberately last — verification is the reason the isolated IP exists,
  and a clean sending reputation is easier to defend once verification pacing is proven.
- "One probe at the start" (ADR-004): every plan is written so that scaling to N probe nodes later
  changes only deployment, never the pacing model — because the token bucket is central from 003 on.
