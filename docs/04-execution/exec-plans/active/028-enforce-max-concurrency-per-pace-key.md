# Plan 028 — Enforce `max_concurrency` per pace key

**Status:** Code complete 2026-10-02 — deploy and gate pending
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
   `pacer.lease_wait` — **default 75 s**, just inside Data Scout's 90 s probe timeout so a wait
   never becomes a transport failure on its side. It is long on purpose: with `conc = 1`, Data
   Scout can have up to 16 domains in flight, and if ten of them are EOP tenants the tenth waits
   ~60 s for the one connection (5-7 s a session). A short wait would turn that queue into
   rechecks. Past the bound the addresses come back unattempted with `retry_after_seconds` from
   the earliest lease expiry — the path a pause already takes, which Data Scout schedules as a
   recheck (plan 073). How often that happens is a gate number, not a guess.
6. **Bands: one connection per pace key, for every key** (operator's decision, 2026-10-01).
   Every seed already says `max_concurrency: 1` and none moves. The reasoning: the rate is set
   by the bucket, not by the connection count, so a second connection to a family adds no
   questions per second — it only overlaps the dead time of the first one's handshake. One
   connection is the quietest shape a receiver can see from us (EOP counts connections per IP,
   not only recipients), and the simplest to reason about.

   **The cost, stated so the gate can measure it.** A session serves one domain: connect +
   banner + EHLO + `MAIL FROM` (1-3 s, no questions asked), then the real `RCPT` and 2-3
   catch-all `RCPT`s at the family's 1/s, then `QUIT`. Of ~5-7 s per domain about 4 s are
   questions, so EOP's effective rate falls from the 1/s the bucket allows to an estimated
   **0.6-0.8/s** — about a quarter to a third slower than day 21. And one slow or timing-out
   tenant holds the family's only connection (head-of-line); EOP is normally fast, so this is a
   number to watch, not a reason to start higher. `conc` can still climb only by promotion
   (plan 012), on evidence from this gate.
7. **Observability.** Gauge `verify_inflight{mx_host=<pace_key>}` (bounded like the other
   per-key gauges by `verify_tracked_mx`); counter
   `verify_lease_waits_total{outcome=granted_after_wait|timed_out}`. The `rcpt_paced` line is
   unchanged; a session line gains `lease_wait_ms`.

**What this does not do.** It raises no rate and changes no verdict. It does not add a
platform-wide connection ceiling — that is Data Scout's `OUTBOUND_SMTP_MAX_INFLIGHT` and stays
there.

## Tasks

- [x] `internal/limiter/lease.lua` + `Lease` / `Release` on the limiter, one round trip each — `internal/limiter/lease.go`; release is a one-line `ZREM` script
- [x] Pacer: `AcquireSession(ctx, mxHost, domain) (release func(), err error)` — pace key,
      `conc` as the limit, fail closed, waits up to `lease_wait`, `PausedError` with the
      earliest expiry past it — returns `(release, waited, err)`: the wait goes to the `session_leased` line
- [x] Prober: take the lease before the dial, release in `defer`; every early-return path — after the resolve (a guarded MX takes no lease), before the token; `SessionLeaser` interface + a compile-time assertion in `main`
- [x] Config: `pacer.session_lease`, `pacer.lease_wait` (75 s); start-up checks that the lease
      outlives the session timeout and that `lease_wait` stays under Data Scout's 90 s probe timeout — **deviation: `lease_wait` defaults to 60 s** and the check is `lease_wait + probe.timeout < 90 s` (`config.DataScoutProbeTimeout`): 75 s + the 20 s session would overrun Data Scout's timeout; `session_lease` 50 s > `probe.timeout`
- [x] Seeds: unchanged — every key `1..1`; a test pins that every shipped seed and `conservative()` say `max_concurrency: 1` — `TestEveryShippedBandAllowsOneConnection`
- [x] Metrics `verify_inflight`, `verify_lease_waits_total`; `lease_wait_ms` on the session log — the session log is a new `session_leased` line (`mx_host`, `pace_key`, `lease_wait_ms`); there was no per-session line to extend
- [x] Tests alongside: — `internal/limiter/lease_integration_test.go` + `lease_test.go`, `internal/pacer/session_test.go`, `internal/prober/session_lease_test.go`, config rows, metrics, `cmd/verifierd` logger. The mxsim `max_concurrent_conns` test became the real-Redis ZCARD test: the set is where concurrency is enforced, and mxsim exposes no concurrency counter to the prober tests
  - real Redis: 32 goroutines over one family key never hold more than `conc` leases at once
    (sampled with `ZCARD` under the race), and a lease from a "crashed" holder expires
  - two pace keys do not share leases (an EOP lease does not block a Google session)
  - Redis down → no dial, no lease (invariant 5)
  - every early-return path in `session` releases (fake limiter counting acquire/release)
  - a throttle lowers `conc` and the next acquire honours the lower limit
  - `lease_wait` exceeded → unattempted with a retry hint, never a verdict (invariant 1)
  - mxsim with `max_concurrent_conns: 1`: N concurrent probes to one profile never trip its
    `TooManyConns` once the band's `conc` is 1
- [x] `docs/06-generated/redis-contract.md` — `rt:mx:<pace_key>:inflight`
- [x] `docs/06-generated/metrics.md` — the two metrics
- [x] `docs/03-engineering/patterns/aimd-pacing.md` — concurrency is enforced, and how
- [ ] Deploy (016's button), between Data Scout warm-up or customer bulk peaks

## Definition of Done

- [ ] **Manual-test gate:** from the node, Data Scout runs two concurrent verify jobs whose
      addresses include ≥ 20 EOP tenants; `ZCARD rt:mx:@microsoft-eop:inflight`, sampled every
      second for the run, never exceeds the family's `conc`, and the run's `rcpt_paced` EOP gaps
      stay ≥ ~1 s (plan 026's gate still holds). Recorded here with the numbers, plus: EOP
      throughput before/after in tokens per minute (day 21's baseline: 0.40/s mean, 57 in the
      busiest minute), the share of requests that hit `lease_wait` and came back as rechecks,
      and the longest single EOP session — so the cost of one connection is on record
- [x] `go test -race -count=1 ./...` green, including the real-Redis tests — 2026-10-02, `VERIFIERD_TEST_REDIS_ADDR=127.0.0.1:56379`
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean, coverage gate ok — 0 issues; limiter 95.2%, pacer 96.9%, prober 97.2%, total 93.1%
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed — fail-closed and central
      bucket mandatory; SSRF guard unaffected (the lease is taken before the vetted dial and
      changes nothing about it) — fail-closed: `TestNoLeaseSetMeansNoSession`, `TestNoLeaseNoDial`; central: the lease set is in the shared Redis; SSRF: the lease is taken after the vetted resolve and changes nothing about the dial; us ≠ address: a lease timeout is `paused`, never a verdict
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added — redis-contract, metrics, observability, aimd-pacing, changelog, ROADMAP
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why a sorted set of leases and not `INCR`/`DECR`.** A counter decremented in a `defer` is
  wrong forever after one crash between the two; a lease with an expiry heals on its own. Same
  reasoning as Data Scout's outbound slots (plan 081).
- **Why enforce `conc` and not `max_concurrency`.** AIMD already moves `conc` on throttles.
  Enforcing the ceiling while ignoring the working value would leave the back-off half of the
  loop decorative, which is the defect this plan exists to remove.
- **Decided 2026-10-01: start every key at `1`.** The `4` first drafted for EOP was anchored on
  one clean day at up to 8, which is evidence of tolerance, not of need. The before/after
  throughput the gate records is what any later promotion is argued from.
- **The way to win the handshake time back without a second connection** — a follow-up, not
  this plan: ask about **several domains in one session** where the receiver allows it. Google
  Workspace shares one MX (`aspmx.l.google.com`) across every hosted domain, so one session can
  `RCPT` addresses of many companies and pay the handshake once. EOP is unproven: each tenant has
  its own MX name over shared front ends, and whether a session opened to tenant A accepts a
  `RCPT` for tenant B (or answers a relay refusal, which is about *us* and must stay `unknown`)
  has to be measured before it is relied on. That changes the HTTP contract (`POST /probe` is
  one MX group today) and Data Scout's grouping, so it is its own plan.
- **The lasting fix for the queue is upstream.** With `conc = 1` per family, the useful number of
  EOP requests in flight from Data Scout is about one; the rest wait inside the verifier. A
  family-aware window on Data Scout's side (don't send a family more than it can take) would
  keep its slots for hosts that can use them. Noted for Data Scout; not in scope here.
- **Data Scout side.** Nothing to change. Its per-job window and platform ceiling stay the
  outer bounds; a lease wait surfaces there as a slower probe or, past `lease_wait`, as a
  rescheduled recheck it already handles.
