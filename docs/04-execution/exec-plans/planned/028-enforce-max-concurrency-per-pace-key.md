# Plan 028 — Enforce `max_concurrency` per pace key

**Status:** Planned (written 2026-10-01)
**Phase:** B
**Depends on:** 003 (central limiter), 026 (pace keys) — both complete

## Goal

Make the band's `max_concurrency` a limit the prober obeys: at most N SMTP sessions open at
once per pace key, counted centrally in Redis, so that "one receiving system" means one rate
**and** one connection budget. Today it is a number the pacer computes, persists and never reads.

## Context

**What exists.** Every band carries `min_concurrency` / `max_concurrency`
(`internal/pacer/band.go`), the AIMD loop moves a working value `conc` between them — `-1` on a
throttle, `+1` on a climb (`pacer.go` `Observe`) — and `persist` writes it to
`rt:mx:<key>:conc`. **Nothing enforces it.** `Pacer.Acquire` takes one *rate* token from the
shared bucket and returns; how many sessions are open to that key at that moment is never
asked. Found on 2026-10-01 while explaining plan 026's result to the operator: the
`@microsoft-eop` band says `max_concurrency: 1`, and on Data Scout's burst day (warm-up day 21)
up to eight EOP tenants were in session at once — one per domain Data Scout had in flight.

**Why it matters, and why it has not hurt yet.** Rate and concurrency are different signals to
a receiver. EOP throttles per sending IP on *connections* as well as on message/recipient rate;
Google documents concurrent-connection limits per IP; small Postfix hosts commonly cap
`smtpd_client_connection_count_limit` at a handful. Plan 026 made the rate shared; the
connection count is still bounded only by Data Scout's platform ceiling
(`OUTBOUND_SMTP_MAX_INFLIGHT`, 16) and its per-job window (8). Day 21 ran 481 EOP tenants that
way with the family `STEADY` and no throttle, so the current exposure is real but unmeasured,
not observed. Two Data Scout jobs at once — which `bulk_max_rows=500` (2026-10-01) makes the
normal case — doubles it to 16.

**Invariants touched.**
- **4 — central.** The connection count must be shared by every node, exactly like the bucket;
  a per-process semaphore would let N nodes open N× the sessions. It lives in the same Redis.
- **5 — fail closed.** A Redis error on the concurrency check means no session, never an
  unpaced one.
- **6 — policy ≠ throttle.** Unchanged: only `IsThrottle()` moves `conc`, as it moves the rate.
- **1 — us ≠ address.** A session refused for want of a concurrency lease is unattempted —
  `ClassPaused`-shaped with a retry hint — never a verdict.

## Design

**A central lease set per pace key, held for the life of one SMTP session.**

1. **Redis.** `rt:mx:<pace_key>:inflight`, a sorted set: member = lease id (random 16 bytes,
   hex), score = lease expiry (unix ms). One Lua script, embedded like `token_bucket.lua`,
   does acquire in one round trip: `ZREMRANGEBYSCORE` the expired leases → `ZCARD` → if below
   the limit, `ZADD` the new lease and `PEXPIRE` the key; else return the earliest expiry as
   the retry hint. Release is `ZREM` of the lease id. A node that dies holding a lease loses it
   at expiry, so a crash cannot wedge a family shut.
2. **Lease TTL** = the prober's session timeout + a margin (config `pacer.session_lease`,
   default session timeout + 30 s), validated at start-up to exceed the session timeout —
   the same "TTL under the longest holder hands a slot out twice" rule Data Scout's plan 097
   enforces for its own slots.
3. **Where it is held.** `Prober.session` acquires the lease **before the dial**, together with
   the first rate token, and releases it in a `defer` next to `conn.Close()`. Catch-all probes
   in the same session spend rate tokens under the lease they already hold. Every early return
   (resolve failure, dial failure, a 4xx/5xx before `RCPT`) still releases — pinned by tests.
4. **The limit is `conc`, not `max_concurrency`.** The AIMD working value already falls on a
   throttle and climbs on clean answers inside `[min_concurrency, max_concurrency]`; enforcing
   it makes the existing loop real rather than adding a second one. A node reads `conc` from
   its own state (rebuilt from `rt:mx:<key>:conc` like the rate); the lease set is shared.
5. **Waiting.** A full lease set waits inside the request, honouring `ctx`, up to
   `pacer.lease_wait` (default 20 s — well inside Data Scout's 90 s probe timeout, so a wait
   never turns into a transport failure on its side). Past that the addresses come back
   unattempted with `retry_after_seconds` from the earliest lease expiry — the path a pause
   already takes, which Data Scout schedules as a recheck (plan 073).
6. **Bands.** Today every seed says `max_concurrency: 1`. Enforcing that as-is would serialise
   EOP to one session at a time — roughly 5-8 s per domain (connect, banner, one real RCPT,
   catch-all RCPTs at 1/s) — cutting EOP throughput by about a third against what day 21
   measured, with no evidence that Microsoft wanted it. So the family seeds move with this plan,
   on day 21's evidence:
   - `@microsoft-eop`: `min_concurrency 1`, `max_concurrency 4` (day 21 ran up to 8 clean);
   - `@google`: `1` / `2`;
   - every other seed and `conservative()`: unchanged at `1` / `1` — one session at a time to a
     host we have not calibrated is the safe default, and Data Scout rarely holds two sessions
     to one small host anyway.
   Raising either further is plan 012's promotion path, on evidence.
7. **Observability.** Gauge `verify_inflight{mx_host=<pace_key>}` (bounded like the other
   per-key gauges by `verify_tracked_mx`); counter
   `verify_lease_waits_total{outcome=granted_after_wait|timed_out}`. The `rcpt_paced` line is
   unchanged; a session line gains `lease_wait_ms`.

**What this does not do.** It raises no rate and changes no verdict. It does not add a
platform-wide connection ceiling — that is Data Scout's `OUTBOUND_SMTP_MAX_INFLIGHT` and stays
there.

## Tasks

- [ ] `internal/limiter/lease.lua` + `Lease` / `Release` on the limiter, one round trip each
- [ ] Pacer: `AcquireSession(ctx, mxHost, domain) (release func(), err error)` — pace key,
      `conc` as the limit, fail closed, waits up to `lease_wait`, `PausedError` with the
      earliest expiry past it
- [ ] Prober: take the lease before the dial, release in `defer`; every early-return path
- [ ] Config: `pacer.session_lease`, `pacer.lease_wait`; start-up check that the lease
      outlives the session timeout
- [ ] Seeds: `@microsoft-eop` concurrency `1..4`, `@google` `1..2`
- [ ] Metrics `verify_inflight`, `verify_lease_waits_total`; `lease_wait_ms` on the session log
- [ ] Tests alongside:
  - real Redis: 32 goroutines over one family key never hold more than `conc` leases at once
    (sampled with `ZCARD` under the race), and a lease from a "crashed" holder expires
  - two pace keys do not share leases (an EOP lease does not block a Google session)
  - Redis down → no dial, no lease (invariant 5)
  - every early-return path in `session` releases (fake limiter counting acquire/release)
  - a throttle lowers `conc` and the next acquire honours the lower limit
  - `lease_wait` exceeded → unattempted with a retry hint, never a verdict (invariant 1)
  - mxsim with `max_concurrent_conns: 1`: N concurrent probes to one profile never trip its
    `TooManyConns` once the band's `conc` is 1
- [ ] `docs/06-generated/redis-contract.md` — `rt:mx:<pace_key>:inflight`
- [ ] `docs/06-generated/metrics.md` — the two metrics
- [ ] `docs/03-engineering/patterns/aimd-pacing.md` — concurrency is enforced, and how
- [ ] Deploy (016's button), between Data Scout warm-up or customer bulk peaks

## Definition of Done

- [ ] **Manual-test gate:** from the node, Data Scout runs two concurrent verify jobs whose
      addresses include ≥ 20 EOP tenants; `ZCARD rt:mx:@microsoft-eop:inflight`, sampled every
      second for the run, never exceeds the family's `conc`, and the run's `rcpt_paced` EOP gaps
      stay ≥ ~1 s (plan 026's gate still holds). Recorded here with the numbers, plus the EOP
      throughput before/after (tokens per minute) so the cost of the limit is on record
- [ ] `go test -race -count=1 ./...` green, including the real-Redis tests
- [ ] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean, coverage gate ok
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed — fail-closed and central
      bucket mandatory; SSRF guard unaffected (the lease is taken before the vetted dial and
      changes nothing about it)
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why a sorted set of leases and not `INCR`/`DECR`.** A counter decremented in a `defer` is
  wrong forever after one crash between the two; a lease with an expiry heals on its own. Same
  reasoning as Data Scout's outbound slots (plan 081).
- **Why enforce `conc` and not `max_concurrency`.** AIMD already moves `conc` on throttles.
  Enforcing the ceiling while ignoring the working value would leave the back-off half of the
  loop decorative, which is the defect this plan exists to remove.
- **Open question — EOP's starting value.** `4` is a guess anchored on one clean day at up to 8.
  Starting at `1` is the cautious alternative and costs about a third of EOP throughput; the
  gate's before/after numbers are what settles it.
- **Data Scout side.** Nothing to change. Its per-job window and platform ceiling stay the
  outer bounds; a lease wait surfaces there as a slower probe or, past `lease_wait`, as a
  rescheduled recheck it already handles.
