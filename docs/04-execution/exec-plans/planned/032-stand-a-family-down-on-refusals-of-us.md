# Plan 032 — Stand a family down on refusals of us

**Status:** Planned (written 2026-10-01)
**Phase:** B
**Depends on:** 026 (pace keys), 030 (refusals of us classed `policy` reliably — this plan is only
as good as the class it counts)

## Goal

When several hosts of **one receiving system** refuse *us* within a short window — Microsoft
tenants answering `S3150`-style blocklist refusals, Google answering "unusual traffic from your
IP", one filtering vendor's customers all naming our IP — stop asking that system for a while,
by itself, and say so where an operator will see it. Today that judgement is made only by Data
Scout's warm-up runner, reading the replies after each part; customer traffic has no such brake.

## Context

**What exists, read from the code:**

- **`PolicyStop`** (`internal/prober/prober.go`, plan 017): ends *one session* after N
  (default 5, `probe.policy_stop`) **consecutive** `ClassPolicy` replies from **that server**. It
  saves the rest of one batch; it knows nothing about other hosts, other requests or time.
- **`iphealth.ObservePolicy(mxHost)`**: every `RCPT`-stage `policy` reply records the raw MX host
  with a timestamp, and **`PolicyHosts(window)`** counts distinct hosts in a window. Its comment
  says it "raises the gauge an operator alerts on" and deliberately "never pauses anything".
  **Nothing calls `PolicyHosts`** — no metric, no log, no endpoint (grep, 2026-10-01). The
  evidence is collected and dropped. It also counts every policy reply alike, so EOP's `5.4.1`
  (a tenant's directory-based edge blocking, not a refusal of us) would dominate it, and it is
  keyed by hostname, so it cannot say "Microsoft" — only "481 different hosts".
- **The node-level stand-down** is `ip_health`: a confirmed listing on a watched DNSBL zone burns
  the node (`ClassIPBurned`) until an operator resumes it. It sees zones; it cannot see a
  provider's private list (Microsoft's, Google's, Proofpoint's), which is exactly where a new
  sender is refused first.
- **The pacer can pause a key** (`StatePaused`, `pause_until` persisted under the pace key since
  026), but only from `Observe(throttled=true)` at the floor of the band. There is no external
  "pause this key for this reason" entry point.
- **Who hears about it on the Data Scout side:** `deploy/healthcheck.sh` pipes
  `deploy/probe-standing.py` into the api container, which reads this service's `/metrics` and
  prints a finding per `ip_health_listed{…} 1`; the health check emails findings. *(The RUNBOOK's
  line that Data Scout emails `/admin/ip-health`'s `reason` is out of date — it reads the metric.)*
  The Telegram notifier on the operator's laptop (`warming/notify-on-exit.sh`) is a warm-up tool
  that reports a runner exiting; it is not a service alert and nothing here should depend on it.

**The warm-up ladder's rules are the prototype.** `warming/run-day.sh` stops on (A) a `policy`
reply naming a list, a zone or our address, and (B) `policy` replies from 5 distinct hosts in a
day with benign wordings excluded (`BENIGN_POLICY_RE`: EOP `5.4.1`, TLS demands, relay denied,
mailbox full …). Over 21 days those two rules were the only thing standing between a provider's
private list and a week of feeding it.

## Design

1. **Count refusals of us per pace key, not per host.** A small `standdown` component in the
   prober path receives every `ClassPolicy` reply (RCPT stage *and* `beforeRCPT`, which today never
   reaches `ObservePolicy`) with its pace key, host and text, and keeps, per pace key, the distinct
   hosts that refused us in a sliding window — in **Redis**, beside the bucket
   (`rt:mx:<pace_key>:refusals`, a sorted set host → last-seen), so two nodes add up (invariant 4).
2. **Only refusals that are about us count.** Excluded, as a shipped list mirroring
   `BENIGN_POLICY_RE`: EOP `5.4.1` / "access denied" at a tenant, TLS demands (until 031 lands they
   are our gap, not our reputation), "relay access denied" / `5.4.4` (the host does not handle the
   domain), mailbox-full wordings. Counted with weight: a reply naming our IP, a blocklist or
   a zone (030's identity rule plus the blocklist prose) counts at once; an unexplained `policy`
   counts only toward the distinct-host threshold.
3. **Stand the key down when N distinct hosts in it refuse us within W.** Defaults: N = 3, W = 30
   minutes, pause P = 6 hours, all config (`standdown.hosts`, `.window`, `.pause`). A single named
   blocklist refusal at a *family* key stands it down immediately (rule A); at a non-family key
   (a lone host) it stands down only that host — exactly the reach the key already has. The pause
   is written through a new `Pacer.PauseKey(ctx, key, until, reason)` into the same
   `rt:mx:<key>:pause_until` the pacer already persists and reads, so every node honours it and a
   restart does not forget it. Paused addresses come back `paused` with an exact
   `retry_after_seconds`, which Data Scout already reschedules.
4. **Why this is safe where `ObservePolicy`'s author refused to pause.** The objection was that any
   one MX answering `5.7.x` could stand the node down. This pauses a **key**, not the node; a lone
   host can only pause itself; a family needs N *distinct* hosts of that family, which one hostile
   or misconfigured server cannot supply. The node-level burn stays where it is, on DNSBL evidence.
5. **Resume:** automatic at `pause_until`; early resume through the existing operator band
   endpoint (`POST /admin/bands/…`) gaining a `resume` action for a key; every stand-down and
   resume is logged with its evidence (the hosts and the replies, redacted).
6. **Alert where someone reads it.** Gauge `verify_key_stood_down{mx_host=<pace_key>}` (1 while
   paused by this rule, bounded by `verify_tracked_mx`) and counter
   `verify_refusals_of_us_total{mx_host=<pace_key>}`. The previously dead `PolicyHosts` is either
   wired to a gauge or removed — one source of truth, not two.

### Data Scout companion

- `deploy/probe-standing.py` prints a finding per `verify_key_stood_down{…} 1`
  (`STOOD DOWN @microsoft-eop`), so the existing health-check email carries it with no new channel.
- `warming/run-day.sh` keeps its own rules: they stop the *ladder*, this stops the *service*, and
  agreeing on the wordings is a matter of sharing the benign list, not merging the two.

## Tasks

- [ ] `standdown` component: per-key distinct-host window in Redis, benign list, rule A / rule B
- [ ] `beforeRCPT` policy replies reach it as well as RCPT-stage ones
- [ ] `Pacer.PauseKey(ctx, key, until, reason)` writing the persisted pause; resume action on the
      admin band endpoint
- [ ] Config `standdown.hosts` / `.window` / `.pause` with validation; `PolicyHosts` wired or removed
- [ ] Metrics `verify_key_stood_down`, `verify_refusals_of_us_total`; stand-down/resume log lines
- [ ] Tests alongside: three EOP tenants refusing with a blocklist wording inside W → `@microsoft-eop`
      paused, a Google request unaffected; three `5.4.1`s → nothing; one host at a non-family key
      → only that host; two nodes (two pacers over one real Redis) add their hosts together; the
      pause survives a restart; Redis down → fail closed as today, no stand-down state invented;
      `paused` results carry an exact retry hint and no verdict
- [ ] `docs/06-generated/redis-contract.md`, `metrics.md`, `api.md` (resume action);
      `docs/07-references/RUNBOOK.md` — stand-down, resume, and the corrected health-check line
- [ ] Data Scout companion: `probe-standing.py` finding

## Definition of Done

- [ ] **Manual-test gate:** against mxsim, three profiles under one test family key answering
      `550 5.7.1 … blocked using zen.spamhaus.org` stand the key down within one batch;
      `verify_key_stood_down` reads 1; Data Scout's `probe-standing.py`, run by hand in the api
      container, prints the finding; the key resumes at `pause_until` without an operator.
      And on live traffic, a week with zero false stand-downs (any stand-down is investigated and
      its evidence recorded here)
- [ ] `go test -race -count=1 ./...` green; vet, gofmt, golangci-lint clean; coverage gate ok
- [ ] `pr-checklist.md` — fail-closed and central confirmed; "us ≠ address" (paused is never a verdict)
- [ ] Docs per Phase 5; `changelog.md`; Status Complete, moved to `completed/`, `ROADMAP.md`

## Notes / decisions / deviations

- **Defaults are a guess anchored on the ladder:** the runner's day-wide limit was 5 distinct
  hosts, and on its worst clean day (17) it reached 4 with none refusing us; a service-wide 30-min
  window at 3 is stricter in time and looser in count. The live week in the gate is what tunes it.
- **Not a reputation oracle.** It reacts to refusals; it cannot see a listing nobody has applied
  yet. SNDS / Postmaster Tools stay the early warning, outside this service.
- **Open question:** whether a family stand-down should also lower the family's band (AIMD floor)
  on resume. Invariant 6 says a policy refusal is not a rate signal, so the plan says no; recorded
  so the question is not re-asked as a fix.
