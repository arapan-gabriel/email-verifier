# Plan 033 — Several domains per session

**Status:** Planned (written 2026-10-01)
**Phase:** B
**Depends on:** 026 (pace keys), 028 (one connection per pace key), 029 (catch-all asked once) —
026 complete, 028 and 029 planned. Has a **Data Scout companion** (below).

## Goal

Pay the SMTP handshake once for several domains that are answered by the same receiving system:
one session to Google's MX asks about addresses at many Workspace domains. With plan 028 holding
each family to one connection, the handshake (connect, banner, EHLO, `MAIL FROM`: 1-3 s with no
question asked) is the main thing standing between a family's one connection and its bucket's
full rate. Google first; EOP only after a measurement proves it answers correctly.

## Context

**What exists.** `POST /probe` is one MX group (ADR-006): `{mx_host, domain, emails[],
need_catch_all, …}` (`internal/api/probe.go`, `DisallowUnknownFields`), validated as one host, one
`domain` (required with `need_catch_all`), ≤ `max_emails`. `prober.Probe` chunks the emails at
`probe.max_rcpt_per_session` (config 50; Data Scout sends ≤ 10 per session,
`VERIFY_SMTP_MAX_RCPT_PER_SESSION`) and stamps **one** catch-all verdict on every result.
`bogusAddress(req.Domain)` builds the bogus local part for that one domain.

Data Scout groups by **domain**, not by MX: `engine._probe_domain` resolves one domain and calls
`smtp_probe.probe_many(answer.hosts[0], addresses, need_catch_all)` once per domain; plan 088 runs
those domain probes concurrently under the per-job window.

**How much there is to gain — measured, warm-up days 19-21 (6,987 answered rows):**
- **Google**: 353 rows on **341 domains** (≈115 a day), over `aspmx.l.google.com` (277),
  `smtp.google.com` (70) and the `alt*`/`googlemail` names (6). One domain per session today.
- **EOP**: 1,519 rows, one tenant per session (day 21: 481 tenants in one run).
At plan 028's one connection per family, ~1-3 s of handshake per domain is ~2-6 minutes a day for
Google and ~8-25 for EOP at warm-up volume, and it scales linearly with customer volume.

**Why Google is safe and EOP is not yet.** Every Workspace domain's MX *is* Google's
(`aspmx.l.google.com` and kin); a session to it accepting `RCPT` for any hosted domain is ordinary
inbound mail behaviour. EOP gives each tenant its own MX name
(`<tenant>.mail.protection.outlook.com`) over shared front ends; whether a session opened via
tenant A's name answers a `RCPT` for tenant B with that tenant's real answer, or with a relay
refusal (`5.7.x` — which is about **us**, `ClassPolicy`, invariant 1 → `unknown`), is not known and
must be measured before anything relies on it.

**Invariants.** 1: a relay refusal of a foreign domain is about us, never `invalid`. 2: the dial
still goes to the one vetted `mx_host`. 4/5: one token per `RCPT`, under the family key, fail
closed. 7: catch-all is per **domain**, so a mixed session needs a verdict per domain; randomiser
stays per host.

## Design

1. **Contract — additive, versioned by presence.** `POST /probe` gains an optional
   `domains: [{domain, emails[], need_catch_all}]`. When present it replaces `domain`/`emails`/
   `need_catch_all` (both at once is a 400). The response shape is unchanged — `results` is
   already keyed by address and every `Result` already carries `catch_all`/`randomiser`. An old
   caller is unaffected; a new caller must not send `domains` to an old service, which
   `DisallowUnknownFields` would 400 — hence the Data Scout flag below, turned on only after this
   deploys.
2. **Who may be grouped is the verifier's decision, per family.** `families.json` (plan 026) gains
   `"multi_domain": true` per entry; only `@google` ships with it. A `domains` request whose
   `mx_host` maps to a family without it is served as **one session per domain** on the same
   connection budget — correct, just not faster — so Data Scout can group optimistically without
   knowing the table.
3. **The session.** One connect/EHLO/`MAIL FROM`; then for each domain its real `RCPT`s; then
   plan 029's catch-all sequence for each domain that had a `250`, with `bogusAddress(domain)`.
   Total `RCPT`s per session stay ≤ `max_rcpt_per_session`; the batch is split across sessions
   by whole domains where possible. Policy-stop still ends the session — for every domain in it —
   since a refusal of our client is not per domain. A relay-shaped refusal (`5.7.1 relay`,
   `5.7.64`, "not permitted to relay") on a foreign-domain `RCPT` is `ClassPolicy` for that
   address and **stops grouping for that family on this node** (counter + log), falling back to
   one domain per session until restart: a receiver that does not accept it gets asked once.
4. **EOP measurement step (no customer traffic).** From the node, with the same tooling plan 026's
   gate used: one session opened to a tenant MX of a domain we control or one already answered
   cleanly, `RCPT` an address at a *second* tenant whose answer is known from Data Scout's
   verdicts. Three outcomes, recorded in this plan: same answer as a direct session → EOP may get
   `multi_domain: true` in a follow-up change; relay refusal → EOP stays one domain per session,
   closed; anything else → closed and noted.
5. **Observability.** `verify_session_domains` histogram (domains per session), counter
   `verify_multi_domain_fallbacks_total{family}`; the session debug line lists domain count, not
   domains.

## Data Scout companion

Not in this repo; its own small Data Scout plan, written when this one is picked up.
- `apps/api/app/core/verify/engine.py` — after resolving, group the batch's domains by
  `answer.hosts[0]`'s **pace family** (a small allowlist mirroring `families.json`'s
  `multi_domain` entries, or simply "same MX host"), and probe a group with one request.
  Profiles are already read for the whole batch up front (plan 088), so `need_catch_all` per
  domain is available before the fan-out.
- `apps/api/app/core/verify/smtp_probe.py` — `_post_probe` builds `domains: [...]` for a group;
  `probe_many` keeps its signature for the single-domain path.
- `app/config.py` — `verify_multi_domain_sessions: bool = False`, **turned on only after the
  verifier with this plan is deployed** (the contract is additive; the flag is the version check).
- Tests: grouping by family; a group's results mapped back to each domain's `_record_domain`
  (catch-all recorded per domain); flag off → today's requests byte for byte.
- No `ENGINE_VERSION` change: the same questions, fewer handshakes.

## Tasks

- [ ] `internal/api/probe.go` — `domains` field, validation (exclusive with `domain`/`emails`,
      every domain non-empty, total ≤ `max_emails`), mapping to the prober
- [ ] `prober.Request` — per-domain groups; `Probe` / `session` iterate them; per-domain catch-all
      verdicts stamped on each domain's results
- [ ] `families.json` + `PaceKey` companion — `multi_domain` flag; `@google` true
- [ ] Relay-refusal detection on a foreign-domain `RCPT` → `ClassPolicy` + node-local fallback
- [ ] Metrics: `verify_session_domains`, `verify_multi_domain_fallbacks_total`
- [ ] EOP measurement (Design 4), recorded here; follow-up change only if it passes
- [ ] Tests alongside:
  - mxsim: one session, three domains, each domain's results and catch-all verdict correct
  - a family without `multi_domain`: one session per domain, results identical
  - foreign-domain relay refusal → `policy` for that address (never `invalid`), fallback engaged
  - policy-stop mid-session ends every domain's remaining `RCPT`s as unattempted
  - tokens: exactly one per `RCPT` across domains, under the family key
  - old-shape request byte-for-byte unchanged; both shapes at once → 400
- [ ] `docs/06-generated/api.md` — the `domains` shape and its rules
- [ ] `docs/06-generated/metrics.md`; ADR-006 note (the seam stays one MX per request)
- [ ] Deploy, then the Data Scout companion with its flag off → on

## Definition of Done

- [ ] **Manual-test gate:** with the Data Scout companion on, a run containing ≥ 30 Google
      Workspace domains: `rcpt_paced` shows those `RCPT`s in ≤ ⌈30 × 2 / 10⌉ sessions rather than
      30, every Google domain's verdict matches what a one-domain session gives (re-probe a sample
      of 10 the old way), and Google's wall clock per domain before/after is recorded. The EOP
      measurement's outcome is recorded either way
- [ ] `go test -race -count=1 ./...` green, including real-Redis tests
- [ ] `go vet ./...`, `gofmt -l .`, `golangci-lint run` clean; coverage gate ok
- [ ] `docs/05-quality/checklists/pr-checklist.md` — us ≠ address (relay refusal), SSRF (one vetted
      host), fail-closed, central bucket
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why the verifier decides grouping, not Data Scout.** Whether a receiver accepts foreign-domain
  `RCPT`s is a property of the receiver, measured here; Data Scout grouping "too much" degrades to
  today's behaviour instead of to wrong answers.
- **Why not one transaction per domain with `RSET` between.** It would be safer on paper, but a
  `RSET` between recipients is itself an unusual pattern for inbound MX and buys nothing a
  per-address `RCPT` answer does not already give; kept as a fallback if a receiver needs it.
- **Harvesting shape.** Many domains in one session is ordinary for a relay delivering a
  newsletter, not for a harvester — but it is still more `RCPT`s per connection, so the existing
  `max_rcpt_per_session` cap is the ceiling and is not raised by this plan.
- **Order.** 029 → 028 → 033. Without 028, Data Scout already overlaps handshakes across domains,
  so 033's gain mostly exists *because* 028 serialises a family onto one connection.
