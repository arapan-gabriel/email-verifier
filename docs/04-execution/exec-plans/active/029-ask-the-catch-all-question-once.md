# Plan 029 — Ask the catch-all question once

**Status:** Code complete 2026-10-02 — deploy and gate pending
**Phase:** B
**Depends on:** 005 (catch-all + randomiser), 026 (pace keys) — both complete. Independent of 028.

## Goal

Stop asking three questions about mailboxes that cannot exist when one answers it. Today every
session that needs a catch-all verdict sends `probe.catch_all_probes` (3) bogus `RCPT`s; a
`RCPT` to a non-existent mailbox is the one question a receiver reads as directory harvesting,
and on Data Scout's warm-up it was most of what we asked. Cut those questions by about 60% with no
change to any verdict a caller can see.

## Context

**What exists** (`internal/prober/prober.go`). `Probe` splits the batch into chunks of
`probe.max_rcpt_per_session`; the **first chunk only** (`i == 0`) runs `session(..., probeCatchAll)`
when `req.NeedCatchAll` and the host is not already a known randomiser (`Profiles.IsRandomiser`).
Inside the session the real `RCPT`s run first, then — unless policy-stop fired — a fixed loop of
`catchAllProbes()` bogus `RCPT`s (default 3, `internal/config` refuses fewer than 2), each spending
a rate token (`p.acquire`). `decideCatchAll(accepted, answered)`: all accepted → catch-all, none →
clean, mixed → randomiser (remembered per host by `mxprofile`, TTL). The verdict is stamped on every
result of the request.

**What Data Scout does with it** (`apps/api/app/core/verify/engine.py`, `smtp_probe.py`,
`services/email_domain_profile_service.py`). It sends `need_catch_all = not knows_catch_all(profile)`
— an established, in-TTL `accept_all` on the domain profile skips the question. It writes the
verdict with `record_catch_all`, which **never writes `None`**, and reads `accept_all` only to
qualify a `250` (`scoring.py`: `smtp_check` true + `accept_all`/`randomiser` → `accept_all`
status). A rejection or a refusal is scored without it.

**What the questions cost — measured, Data Scout warm-up days 19-21 (6,987 answered addresses):**

| | answered | real `RCPT` accepted | `catch_all=false` | `catch_all=true` | randomiser |
|---|---|---|---|---|---|
| all | 6,987 | 6,680 (96%) | 5,818 | 1,108 | **1** |
| EOP (`*.mail.protection.outlook.com`) | 1,519 | 1,475 (97%) | **1,519** | 0 | **0** |

Day 21 part 3 from the `rcpt_paced` journal: **8,594 tokens for 2,176 addresses (3.95 each)** —
one real `RCPT` and ~2.95 bogus ones per address, since the warm-up is one address per domain.
EOP: 1,942 tokens for 481 tenants (4.04 each), and **every one of those tenants rejected all three
bogus `RCPT`s**: the second and third were asked after the answer was already in.

Two corrections to what the code's own comments assume, both from that table:
- The randomiser was documented as "a Microsoft property" (invariant 7's wording, `api.md`). Over
  three days EOP never randomised, and the one randomiser in 6,987 rows was elsewhere. Rare, and
  not where the cost is.
- The catch-all question only ever matters to a `250`. At 96% acceptance that is nearly every
  session on the warm-up pool, so "skip when nothing was accepted" alone saves ~4% — real, but the
  saving is in stopping early, not in skipping.

**Invariants.** 1 (us ≠ address): bogus answers never become a verdict on a real address —
unchanged. 7 (catch_all + randomiser): both fields keep their meaning; a skipped question leaves
them `null`, which Data Scout already reads as "not established" and never stores. 4/5: every bogus
`RCPT` still takes a token, fail-closed.

## Design

**Sequential probes with early stop, asked only when a `250` needs them.**

1. **Ask only if an answer depends on it.** The catch-all verdict qualifies a `250` and nothing
   else. If no real `RCPT` in the session was accepted (all `invalid`, `policy` — e.g. a tenant
   answering `5.4.1` to everything — `deferred`, `throttled`), skip the bogus probes: `catch_all`
   and `randomiser` stay `null`. Data Scout writes nothing and asks again next time, exactly as for
   a session that died. This also covers the DBEB tenant without a separate list: its real reply is
   `ClassPolicy`, so nothing was accepted.
2. **Ask in the first chunk that has a `250`, not in chunk 0.** Today a request whose first chunk
   was all rejections and whose second had a `250` never establishes the verdict for that `250`.
   The decision moves to "the first session in this request with an accepted real address".
3. **Stop at the first rejection.** One bogus `RCPT`:
   - **rejected** → `catch_all=false, randomiser=false`. Stop. (83% of the table above.)
   - **accepted** → ask a second: accepted again → catch-all; rejected → randomiser. Two, not three:
     two disagreeing answers are already the randomiser's definition.
   `probe.catch_all_probes` becomes the **ceiling** of that sequence (default 2, validator ≥ 2).
4. **The cost of stopping early is a missed randomiser, and it is bounded and measured.** A host
   that answers by coin flip rejects the first bogus `RCPT` about half the time and is then
   recorded clean. At the observed rate (1 in 6,987) that is a few verdicts a month. To keep the rate
   *measured* rather than assumed, an **audit sample**: with probability `probe.catch_all_audit_rate`
   (default 0.05) a session asks the full old sequence of 3 regardless. Its outcome counts like any
   other and feeds a counter, so a family that starts randomising shows up in the metric before it
   shows up in a customer's list. A host found randomising is still remembered per host
   (`mxprofile`), so the next domain on it skips probing entirely, as today.
5. **No HTTP change, no new Redis key.** Same request, same response, same tri-state fields. The
   audit sample needs no state.
6. **Observability.** Counter `verify_catch_all_probes_total{outcome}` with
   `skipped_no_accept | clean_after_1 | catch_all_after_2 | randomiser_after_2 | audit_full`
   (bounded), and the bogus count on the session's debug line.

**Expected effect** (from the table, warm-up shape — one address per domain): bogus `RCPT`s per
session from 3 to ≈ 0.96 × (0.83 × 1 + 0.16 × 2) + audit ≈ **1.15**; tokens per address from 3.95 to
**≈ 2.2 (−45%)**; `RCPT`s to non-existent mailboxes **≈ −60%**. EOP: 4.04 tokens per tenant to
**≈ 2.05 (−50%)** — with plan 026's single bucket that is EOP's wall clock halved, and with plan
028's single connection it is most of what 028 costs, given back.

**Data Scout side: nothing to change.** `need_catch_all` keeps its meaning; a `null` verdict is
already "not established, never stored, ask next time". **No `ENGINE_VERSION` bump**: a stored
verdict means exactly what it meant (the same question, answered with fewer samples). The one
semantic difference — a randomiser can now be recorded clean with ~50% probability per domain
instead of ~12.5% — is stated in Data Scout's changelog when this deploys.

## Tasks

- [x] `session`: bogus loop becomes sequential with early stop (rejected → stop; accepted → one
      more; the ceiling is `catchAllProbes()`), and runs only when a real `RCPT` in that session
      was accepted
- [x] `Probe`: catch-all asked in the first session with a `250`, not unconditionally chunk 0
- [x] Audit sample: `probe.catch_all_audit_rate` (config + env), full sequence of 3 when drawn;
      injectable randomness for tests
- [x] Config: `catch_all_probes` default 2 (validator ≥ 2 kept), `catch_all_audit_rate` in [0, 1]
- [x] Metric `verify_catch_all_probes_total{outcome}`
- [x] Tests alongside (`internal/prober/catchall_test.go`; counts on scripted servers, verdicts also on the existing mxsim `gmail`/`catchall` integration tests; config and metrics tests too):
  - clean host: exactly 1 bogus `RCPT` (mxsim `gmail` profile), verdict `false/false`
  - catch-all host: exactly 2, verdict `true/false` (mxsim `catchall`)
  - randomiser stub (accept, reject): 2, verdict `true/true`, host remembered
  - no real `250` (all `5.1.1`; all `5.4.1`): 0 bogus, both fields `null`
  - first chunk all rejected, second chunk has a `250`: verdict established in chunk 2
  - audit drawn: 3 asked regardless of the first answer
  - every bogus `RCPT` still takes a token; Redis down → no bogus `RCPT` (invariant 5)
  - a bogus answer never changes a real address's class (invariant 1)
- [x] `docs/06-generated/api.md` — the catch-all paragraph (sequence, skip rule, randomiser wording)
- [x] `docs/06-generated/metrics.md` — the counter
- [x] `CLAUDE.md` invariant 7 + `ARCHITECTURE.md`: drop "a randomiser (Microsoft)" — say "a
      randomising server", per the measured table
- [ ] Deploy (016's button), between Data Scout warm-up or bulk peaks

## Definition of Done

- [ ] **Manual-test gate:** a Data Scout run of ≥ 500 addresses (a warm-up-sized part, or a
      customer-shaped batch) after deploy, compared with day 21 part 3 from `rcpt_paced`:
      tokens per address ≤ 2.4 (day 21: 3.95), EOP tokens per tenant ≤ 2.3 (day 21: 4.04), the
      `accept_all` share of accepted addresses within ±3 points of days 19-21's 16.6%, and the
      metric's `outcome` split recorded here
- [x] `go test -race -count=1 ./...` green, including real-Redis tests — 2026-10-02, `VERIFIERD_TEST_REDIS_ADDR` set
- [x] `go vet ./...`, `gofmt -l .`, `golangci-lint run` clean; coverage gate ok — 0 issues; prober 97.0%, total 93.0%
- [x] `docs/05-quality/checklists/pr-checklist.md` — us ≠ address and fail-closed mandatory — us ≠ address: bogus answers never reach a real result (test); fail-closed: a refused token ends the sequence, no bogus RCPT without budget (test); SSRF and tcp4 untouched
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added (and Data Scout's, for the
      randomiser-miss note)
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why not just set `catch_all_probes: 1`.** One probe cannot tell a catch-all from a randomiser
  that happened to say yes, and the validator refuses it for that reason. Early stop keeps the
  second sample exactly where it is informative — after a `250` to a bogus address.
- **Why no per-tenant "DBEB" list.** Rule 1 already skips a tenant whose real reply is `5.4.1`, and
  a list would need a key, a TTL and an eviction story for a saving rule 1 gets for free.
- **Ordering with 028.** Independent, but 029 first is the better order: it halves the tokens
  028's single connection has to carry, so 028's gate measures the cheaper shape.
- **What this does not do.** It does not skip the *real* `RCPT` at hosts where a `250` cannot
  mean anything (a known catch-all or randomiser) — that changes stored verdicts (`smtp_check:
  null`) and belongs to Data Scout's product decision noted in its plan 088.
