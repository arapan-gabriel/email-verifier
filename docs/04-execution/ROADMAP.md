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
| 010 | ip-health | blocklist self-monitoring, **off unless a DNSBL-capable resolver is named and passes a self-test**; a listing pauses, a policy rate only alerts | ✅ host stub refused without pausing; listing burns the node; resume needs no redeploy |
| 011 | suppression | a **digest-only** second line — Data Scout checks the authoritative list three times before calling. Pushed to `POST /admin/suppress`; addresses never stored | ✅ address and domain refused before any socket; Redis holds two hashes and no `@` |
| 012 | band-promotion | the gap AIMD cannot close: it never climbs past a guessed ceiling. Clean answers **at** the ceiling become a bounded proposal; a person promotes it. The lab's active ladder stays in the lab | ✅ proposal 4→6 from evidence; promoted, cleared, no restart; nothing raised automatically |
| 013 | deployment | OVH host, systemd + distro Redis on a unix socket, mTLS boundary, preflight as a hard `ExecStartPre`. The gate itself had to be fixed first — it was grading the IPv6 identity and reading the EHLO greeting as the `RCPT` reply | ✅ preflight GO on the real host; no certificate → TLS alert, not 401; live probe returns `source_ip` = egress. Firewall stays shut: who may reach the API is 008's call |
| 016 | release-and-deploy | the half of 013 left undone: a `workflow_dispatch` deploy of the artifact CI actually tested, a `deploy` user whose sudoers holds one root-owned script, and a rollback that tells a bad release from a broken host identity — the previous binary fails the same start gate | ✅ deployed from the button, hash changed from the laptop build to CI's; broken binary rolls itself back; forced `NO-GO` does **not** roll back; tampered bundle refused |
| 018 ✅ | ~~explain-a-verdict~~ **done 2026-09-10** | the warm-up holds on `block` moving, `block` moved, and nothing recorded said which kind it was. One `smtp_reply` line per verdict that needs explaining, redacted **before** truncation — the other order leaves the local part behind — plus the same three fields in Data Scout's `signals` | ✅ a live M365 `5.4.1` explained from the journal and from the row; no local part in any info line, including five shapes of quoted address |
| 019 | envelope-sender-isolation | `mail_from` moves to `verify@probe.datascoutmail.com`. Plan 013 shipped the identity split as a table and could not switch it on — the sub-domain had SPF but no MX, so sender-domain verification answered `554 5.1.8`. Only that lookup was ever missing: the callout half had been answering `250` since before the record existed | a live probe from `92.222.87.97` with the new sender returns a verdict about the *recipient* — never `554 5.1.8`, never `unknown` on account of us |
| 017 | session-limits-and-per-recipient-policy | the cut-over found it at 2,000/day: M365 answers `452 4.5.3 Too many recipients` on the *second* recipient, which classes as throttling — so the pacer is told the rate is wrong when the session is full, and five of six addresses come back `unknown` while Microsoft's rate halves five times. Learn the limit per host, reconnect, and keep it out of the throttle signal | a host capping at *n* answers a batch of `n×3` in three sessions with nothing left `unknown`, and **its rate is unchanged across the event** |

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
