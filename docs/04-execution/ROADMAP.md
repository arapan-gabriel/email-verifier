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
| 002 | ~~ssrf-guard-and-safety~~ **done 2026-08-28** | deny-by-default guard between the lookup and the socket; prober takes vetted IPs, never a hostname | ✅ loopback, private, link-local, v4-mapped and `localhost` all refused with zero SYNs; real MX unaffected |
| 003 | ~~central-redis-limiter~~ **done 2026-08-28** | shared bucket is THE limiter; per-MX AIMD; fail-closed | ✅ 12 recipients at 1.99/s against a 2/s band; Redis stopped → `no_budget`, zero connections opened |
| 004 | ~~dns-resolver-and-cache~~ **done 2026-08-28** | configurable resolvers + own timeout; in-process TTL cache of *vetted* results and refusals (not Redis — see the contract) | ✅ 10 resolutions = 1 lookup; refusals cached; bounded; literals bypass |
| 005 | ~~catch-all-and-randomisers~~ **done 2026-08-28** | N bogus probes tell a catch-all from a coin flip; the randomiser verdict is per **server** and remembered | ✅ catch-all → `catch_all:true`; coin-flip host → `randomiser:true`, remembered, neighbours condemned with zero probes |
| 006 | ~~greylist-retry~~ **done 2026-08-28** | no queue here — a retry's answer has nowhere to land (ADR-003/006). `retry_after_seconds`, exact for a paused MX, clamped otherwise | ✅ defers with a usable hint; same tuple after the window resolves; answers carry no hint |
| 007 | ~~policy-stop~~ **done 2026-08-28** | all that ADR-006 left of `bulk-verify-and-queue`: N **consecutive** policy replies end the session; orchestration is Data Scout's Celery | ✅ trips at the threshold, budget stops with it, an isolated `5.7.x` does not trip it |
| 008 | data-scout-integration | Data Scout `email_verify.py` → HTTP client to this service; retire in-process `smtp_probe` | Data Scout verify endpoint returns this service's verdict end-to-end |

## Phase B — Operations & hardening

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 009 | observability | hand-rolled Prometheus text (no client library), request-id logging, **and a bound on the pacer's in-memory map** — which was also the cardinality bound | ✅ scrape shows results, replies, blocked reasons and per-MX gauges; `verify_tracked_mx` is the canary |
| 010 | ip-health | blocklist self-monitoring, **off unless a DNSBL-capable resolver is named and passes a self-test**; a listing pauses, a policy rate only alerts | ✅ host stub refused without pausing; listing burns the node; resume needs no redeploy |
| 011 | suppression | a **digest-only** second line — Data Scout checks the authoritative list three times before calling. Pushed to `POST /admin/suppress`; addresses never stored | ✅ address and domain refused before any socket; Redis holds two hashes and no `@` |
| 012 | band-promotion | the gap AIMD cannot close: it never climbs past a guessed ceiling. Clean answers **at** the ceiling become a bounded proposal; a person promotes it. The lab's active ladder stays in the lab | ✅ proposal 4→6 from evidence; promoted, cleared, no restart; nothing raised automatically |
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
