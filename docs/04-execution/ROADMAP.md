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
| 008 ✅ | ~~data-scout-integration~~ **done 2026-09-10** | the cut-over: `probe_many` is an mTLS+token HTTP client to `POST /probe`, and **the main product host opens no outbound `:25`** — the reputation goal the project exists for. Live since 2026-09-08, `ENGINE_VERSION` 4 | ✅ in production, not staging: 200+ verdicts carrying `source_ip: 92.222.87.97`; greylist retry proven live on a server's own 900s hint. The ladder, their finder test and the two defects the cut-over found (017, 018) are reassigned, not waited on |

## Phase B — Operations & hardening

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 009 | observability | hand-rolled Prometheus text (no client library), request-id logging, **and a bound on the pacer's in-memory map** — which was also the cardinality bound | ✅ scrape shows results, replies, blocked reasons and per-MX gauges; `verify_tracked_mx` is the canary |
| 010 | ip-health | blocklist self-monitoring, **off unless a DNSBL-capable resolver is named and passes a self-test**; a listing pauses, a policy rate only alerts  **Live since 2026-09-11** (plan 020): unbound on the node, clean on all zones.| ✅ host stub refused without pausing; listing burns the node; resume needs no redeploy |
| 011 | suppression | a **digest-only** second line — Data Scout checks the authoritative list three times before calling. Pushed to `POST /admin/suppress`; addresses never stored  **Live since 2026-09-11** (plan 020): hourly digest export from Data Scout, enforced.| ✅ address and domain refused before any socket; Redis holds two hashes and no `@` |
| 012 | band-promotion | the gap AIMD cannot close: it never climbs past a guessed ceiling. Clean answers **at** the ceiling become a bounded proposal; a person promotes it. The lab's active ladder stays in the lab | ✅ proposal 4→6 from evidence; promoted, cleared, no restart; nothing raised automatically |
| 013 | deployment | OVH host, systemd + distro Redis on a unix socket, mTLS boundary, preflight as a hard `ExecStartPre`. The gate itself had to be fixed first — it was grading the IPv6 identity and reading the EHLO greeting as the `RCPT` reply | ✅ preflight GO on the real host; no certificate → TLS alert, not 401; live probe returns `source_ip` = egress. Firewall stays shut: who may reach the API is 008's call |
| 016 | release-and-deploy | the half of 013 left undone: a `workflow_dispatch` deploy of the artifact CI actually tested, a `deploy` user whose sudoers holds one root-owned script, and a rollback that tells a bad release from a broken host identity — the previous binary fails the same start gate | ✅ deployed from the button, hash changed from the laptop build to CI's; broken binary rolls itself back; forced `NO-GO` does **not** roll back; tampered bundle refused |
| 018 ✅ | ~~explain-a-verdict~~ **done 2026-09-10** | the warm-up holds on `block` moving, `block` moved, and nothing recorded said which kind it was. One `smtp_reply` line per verdict that needs explaining, redacted **before** truncation — the other order leaves the local part behind — plus the same three fields in Data Scout's `signals` | ✅ a live M365 `5.4.1` explained from the journal and from the row; no local part in any info line, including five shapes of quoted address |
| 019 | envelope-sender-isolation | `mail_from` moves to `verify@probe.datascoutmail.com`. Plan 013 shipped the identity split as a table and could not switch it on — the sub-domain had SPF but no MX, so sender-domain verification answered `554 5.1.8`. Only that lookup was ever missing: the callout half had been answering `250` since before the record existed | a live probe from `92.222.87.97` with the new sender returns a verdict about the *recipient* — never `554 5.1.8`, never `unknown` on account of us |
| 017 ✅ | ~~session-limits-and-per-recipient-policy~~ **done 2026-09-10** | `find_max_candidates` is 6 and `policy_stop` is 5, so the guard fired one candidate before the ladder ended at every M365 tenant — two settings in two repositories that met only at a customer-visible failure. The caller now carries its own ceiling, **clamped, never refused**. The session-limit half is descoped: `452 4.5.3` did not reproduce | ✅ live M365: 5 of 6 asked at the default, 6 of 6 with the caller's ceiling, and a request asking for 1000 gets the configured 10 |
| 020 ✅ | ~~turn-on-ip-health-and-suppression~~ **done 2026-09-11** | 010 and 011 were ✅ and switched off. Both are on. Enabling suppression exposed a chicken-and-egg nobody could see by reading: the import endpoint existed only once enforcement was on, so the first export 404'd — one method was answering both "configured" and "refuse probes" | ✅ Spamhaus's test point answers `127.0.0.10` locally and `127.255.255.254` through 1.1.1.1; a suppressed digest gives `class=suppressed, connected=false, accepted=null` with no socket |

## Phase C — Outbound relay (send mail from the isolated IP)

| # | Plan | Delivers | Manual-test gate |
|---|---|---|---|
| 014 | relay-outbound-mail *(rewritten 2026-09-11)* | `POST /send`, DKIM-signed, durable queue, through the **same** pacer as verification. Data Scout already has an outbox and a provider seam — production sends password resets from a personal Gmail today, which is what this replaces. **Blocked on 020** (its two safety checks are currently disabled) and on 015's return-path decision, which fixes the envelope sender | a signed message passes SPF+DKIM+DMARC at mail-tester; suppressed / unhealthy / stale-list all refuse to send; verify still never sends `DATA`; a restart loses no queued message |
| 015 | relay-bounce-handling *(rewritten 2026-09-11)* | the original assumed a bounce mailbox that does not exist and cannot simply be made: this host must not receive mail, and the domain is *forwarded* by Cloudflare Email Routing, not stored. Three options weighed; **Email Worker → `POST /bounce`** recommended, VERP so a bounce identifies its message without parsing the body | a real bounce from a dead address arrives, parses, and suppresses on both sides; a complaint spike pauses **sending** and not verification |

## Notes

- Phases are ordered by dependency, not just priority. 001 needs 000; 003 needs 001; 008 needs
  003–005. Phase C (relay) is deliberately last — verification is the reason the isolated IP exists,
  and a clean sending reputation is easier to defend once verification pacing is proven.
- "One probe at the start" (ADR-004): every plan is written so that scaling to N probe nodes later
  changes only deployment, never the pacing model — because the token bucket is central from 003 on.
