# Plan 026 — Pace by provider, not by hostname

**Status:** Code complete 2026-09-30 — deploy and the node gate pending (written 2026-09-21)
**Phase:** B
**Depends on:** 003 (central limiter), 012 (band promotion) — both complete

## Goal

Make every hostname that fronts **one receiving system** share **one** token bucket and one band,
so a batch spread over many Microsoft 365 tenants, Google Workspace domains or one filtering
vendor's customers is paced as the single system it reaches. Today each tenant hostname is its own
bucket, so the rate limit that invariant 4 exists to enforce does not apply to the largest receiver
in our traffic.

## Context

**What exists.** Every piece of pacing state is keyed by the exact MX hostname:
`rt:mx:<mx_host>:bucket`, `:rate`, `:state`, `:pause_until` and `limits:mx:<mx_host>`
(`docs/06-generated/redis-contract.md`). `Pacer.Acquire(ctx, mxHost, domain)` takes **one token
per `RCPT`** from that host's bucket (`internal/prober/prober.go`, "one token per recipient"), and
`bandFor` resolves the band as `limits:mx:<host>` → the shipped seed for the **recipient domain**
→ `conservative()` (0.1–0.5/s, `internal/pacer/band.go`).

**Why that is wrong for the receivers that matter.** A hostname is not a receiving system:

| receiver | hostnames it presents | today |
|---|---|---|
| Microsoft 365 / Exchange Online Protection | one per tenant: `<tenant>.mail.protection.outlook.com` | **one bucket per tenant — no shared limit at EOP** |
| Google Workspace | `aspmx.l.google.com` + `alt1…alt4.aspmx.l.google.com` | one bucket per host (up to five), **band = `conservative()`**, because the seed is looked up by the customer's domain, not by Google |
| Hornetsecurity, Proofpoint (`*.pphosted.com`), Mimecast | shared or per-customer hosts | per host |
| IONOS | `mx00.ionos.de`, `mx01.ionos.de` | two buckets for one system |

**Measured on Data Scout's warm-up ladder, day 11 (2026-09-21, 1,595 addresses):**
**437 addresses (27%) sat behind `*.mail.protection.outlook.com`, each tenant a distinct host**;
1,005 distinct MX hosts in the day. Google: 86, of which 67 on `aspmx.l.google.com`. IONOS:
74 + 55 over two hosts. The ladder never felt this because it sent parts of 50–80 addresses with
20–30 minute pauses, which spread EOP across the whole day by hand. **A customer's bulk upload of
2,000 German SMB addresses has no pauses**: ~540 EOP sessions would open as fast as Data Scout's
concurrency allows, every one of them the first in its own bucket, so every one of them allowed.

**Invariants touched.**
- **4 — the rate budget belongs to the recipient MX, and the bucket is central.** This plan
  keeps the bucket central and changes what "the recipient MX" means: the *system* that answers,
  not the string in the MX record. The wording in `CLAUDE.md` and `ARCHITECTURE.md` is sharpened in
  the same change; the number and the rule stay.
- **5 — fail closed.** Unchanged: the family key goes to the same `Take`; a Redis failure is still
  no probe.
- **6 — `ClassPolicy` is never throttling.** Matters more once a bucket is shared: one tenant's
  `550 5.4.1` (directory-based edge blocking — a tenant setting, not a rate signal) must not slow
  EOP for every other tenant. It already cannot, because `Observe` takes only
  `Class.IsThrottle()`. A test pins that for a family key.
- **7 — randomiser.** `mx:<host>:randomiser` **stays per host.** Whether EOP answers by coin flip
  is a property of the tenant's configuration, so condemning every tenant for one would be wrong.
  Noted, not changed.

## Design

**One new function, one lookup order, no new endpoint.**

1. `internal/pacer/family.go` — `PaceKey(mxHost string) string`. A shipped, ordered suffix table
   (`go:embed`, `internal/pacer/families.json`, same shape of reasoning as `bands/*.json`):

   | suffix | key |
   |---|---|
   | `.mail.protection.outlook.com` | `@microsoft-eop` |
   | `.aspmx.l.google.com`, `aspmx.l.google.com`, `.googlemail.com` | `@google` |
   | `.pphosted.com` | `@proofpoint` |
   | `.mimecast.com`, `.mimecast.co.za` | `@mimecast` |
   | `.hornetsecurity.com` | `@hornetsecurity` |
   | `.ionos.de`, `.kundenserver.de` | `@ionos` |

   Anything unmatched returns the hostname itself — **today's behaviour exactly**. Matching is on
   the lower-cased, dot-trimmed host, suffix on a label boundary (`evil-outlook.com` does not
   match `.outlook.com`). The `@` prefix cannot occur in a hostname, so a family key never collides
   with a real host's keys.

2. **Every per-MX call site uses the key, not the host**: `Acquire`, `Observe`, `stateFor`,
   `persist`, the limiter's `Take`, `ProposalKey`, pause bookkeeping and the per-MX metrics. The
   prober keeps passing the real host; the pacer maps it once at the boundary, so the SSRF guard,
   the dial and the journal still see the real hostname.

3. **Band lookup order** becomes: `limits:mx:<key>` → **seed for the family** (`bands/@<family>.json`,
   new: `@microsoft-eop` and `@google` start from the existing `outlook.com` / `gmail.com` seeds,
   0.5–1.0/s, concurrency 1) → seed for the recipient domain → `conservative()`. This also fixes the
   Workspace case: a company on Google today gets 0.1–0.5/s as a stranger.

4. **Redis.** No new key *shapes*: `rt:mx:<key>:*` and `limits:mx:<key>` with `<key>` now possibly
   `@family`. Old per-tenant `rt:mx:*.mail.protection.outlook.com:*` keys are simply no longer
   read; they are operational state with nothing that needs migrating. A per-host
   `limits:mx:<host>` an operator promoted under 012 is **not** consulted for a host that now maps
   to a family — listed in the task below so a promotion is not silently lost.

5. **Observability.** The per-MX gauges are labelled by the pace key, so `verify_tracked_mx`
   drops (1,005 hosts on day 11 → a few hundred keys) and EOP shows as one series. The `smtp_reply`
   journal line gains `pace_key=` next to `mx_host=` so a pause can be traced to the family that
   caused it. **No HTTP change**: Data Scout's `signals.mx_host` keeps the real host.

**What this does not do.** It does not raise any rate. For a 2,000-address all-EOP batch the cost is
explicit: 2,000 tokens at 0.5–1.0/s is **35–70 minutes** — the same as 2,000 `@gmail.com` addresses
already take — instead of the unbounded speed it has today. That is the point. Raising the EOP band
is plan 012's promotion path, on evidence.

**Deviation from the lab.** `ds-smtp-retry` keys by host because it calibrated one provider at a
time and never met a tenant farm. Recorded here, not ported back.

## Tasks

- [x] Check every `limits:mx:*` key on the node (`redis-cli --scan --pattern 'limits:mx:*'`) and list — **done 2026-09-30**: the node holds only `limits:mx:92.222.87.97` (+ `:proposed`), our own IP from the self-test; nothing to carry
      any promoted band on a host that will map to a family; carry it to the family key or record why
      not
- [x] `internal/pacer/families.json` + `PaceKey` with label-boundary suffix matching — plus `smtp.google.com` under `@google` (deviation, see changelog)
- [x] Route `Acquire` / `Observe` / `stateFor` / `persist` / `ProposalKey` / pause metrics through
      `PaceKey`; limiter `Take` receives the key
- [x] Band order: key → family seed → domain seed → conservative; seeds `bands/@microsoft-eop.json`,
      `bands/@google.json`
- [x] `pace_key=` on the `smtp_reply` journal line
- [x] Tests alongside: — `internal/pacer/family_test.go`, `redis_integration_test.go::TestRealFamilyNeverOverAdmitsAcrossTenants` (real Redis, two tenants, exactly the burst), `mxprofile_test.go::TestRandomiserStaysPerTenant`. The two-listener mxsim shape became the real-Redis two-tenant test: the bucket is where aggregation happens, and the prober takes one port per instance
  - two EOP tenant hosts draw from one bucket — aggregate rate across both ≤ band max (mxsim, two
    listeners, the 003 gate's shape)
  - an unmatched host keeps its own bucket, key == hostname (no behaviour change)
  - `evil-outlook.com`, `outlook.com.attacker.net`, mixed case and trailing dot
  - a `ClassPolicy` `5.4.1` from one tenant leaves the family's rate untouched (invariant 6)
  - Redis down → `Acquire` on a family key fails closed (invariant 5)
  - a Workspace domain with no domain seed gets the `@google` band, not `conservative()`
  - randomiser stays per host: marking one tenant leaves a sibling tenant unmarked
- [x] `docs/06-generated/redis-contract.md` — `<mx_host>` may be an `@family` key; the table
- [x] `docs/06-generated/metrics.md` — per-MX labels are pace keys
- [x] `docs/03-engineering/patterns/aimd-pacing.md` — families, and why the randomiser is not one
- [x] `CLAUDE.md` + `docs/02-architecture/ARCHITECTURE.md` — invariant 4's wording: the receiving
      system, not the hostname
- [ ] Deploy (016's button) and run the gate

## Definition of Done

- [ ] **Manual-test gate:** from the node, one `POST /probe` sequence covering **≥ 20 distinct
      `*.mail.protection.outlook.com` tenants** leaves exactly one `rt:mx:@microsoft-eop:bucket`, no
      new per-tenant `rt:mx:*` keys, and an aggregate `RCPT` rate within the `@microsoft-eop` band
      (read from the `smtp_reply` journal timestamps); a non-family host in the same run keeps its own
      key. Recorded here with the numbers
- [ ] **Downstream gate (Data Scout plan 083):** the burst day of the hold week — one uninterrupted
      batch of ~2,000 from the warm-up pool — runs clean on the ladder's stop rules with this deployed
- [x] `go test -race -count=1 ./...` green — 2026-09-30, with `VERIFIERD_TEST_REDIS_ADDR` (make test-redis)
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean — 0 issues; `scripts/coverage-gate.sh` ok (pacer 96.7%, total 92.8%)
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed — fail-closed (a family key is
      still a `Take`), SSRF guard unaffected (the dial still uses the vetted real host), "us ≠
      address" unaffected (no classification change)
      — confirmed 2026-09-30. us ≠ address, SSRF, IPv4: no classification or dial change. Fail-closed: `TestFamilyKeyFailsClosed`. Central bucket: the family bucket is the shared one. `policy` does not drive the pacer: `TestAPolicyAnswerFromOneTenantLeavesTheFamilyRate`. **One note on §2 "no package-level mutable state"**: `families` is package-level, parsed once from the embedded file and never written after — the same shape as the existing `seed embed.FS`; no `init()`
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added — redis-contract, metrics, aimd-pacing, CLAUDE.md + ARCHITECTURE invariant 4, changelog, ROADMAP
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why a suffix table and not the MX's IP or ASN.** Grouping by resolved IP/ASN would catch
  families nobody listed, but EOP answers from rotating pools, so the bucket would split again, and
  a hosting ASN groups unrelated small servers that should keep their own budgets. A short table of
  known farms is explicit and reviewable; an unknown farm costs what it costs today, not more.
- **Why the family seed does not simply reuse the domain seed.** A company on Workspace is not
  `gmail.com`; the lookup has to go through the MX to find Google at all. The consumer seeds stay
  for consumer domains.
- **Data Scout side.** Nothing to change there: its only per-probe control is the platform-wide
  daily ceiling (`_take_probe_budget`, `verifyday:<date>`), and per-MX pacing already lives here.
  Its plan 083 records this plan as the precondition for customer bulk traffic.
- **The M365 `5.4.1` cost** (a tenant that answers every `RCPT` with Access denied, so a session
  proves nothing) is a separate saving — skipping such tenants early — and is not in scope.
