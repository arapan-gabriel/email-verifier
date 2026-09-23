# Plan 027 — Test-coverage audit and hardening

**Status:** Complete — signed off 2026-09-22 (written and implemented the same day). Two DoD items can only be satisfied on GitHub and are recorded below with how they were verified locally.
**Phase:** B
**Depends on:** none to start. Sequenced **after 026**, which rewrites the pacer's keying; the
pacer tests here go through `Acquire`/`Observe`, not raw key strings, so 026 does not have to
rewrite them. Touches the same classifier as active plans 022 and 024. Their tests stay as they are
and the new property tests sit next to them.

## Goal

Measure what the suite really exercises. Close the gaps on the paths that decide a verdict, spend a
recipient MX's budget or guard the edge. Turn the result into a CI gate that stops coverage going
down again. The audit already found two defects that no existing test catches, one of them against
invariant 1. That is the argument for doing this before customer bulk traffic (Data Scout plan
`083`) multiplies whatever the suite misses.

## Context

**What exists.** 31 `*_test.go` files against 42 non-test `.go` files and about 2,200 statements of
service code. The gate is `go test -race -count=1 ./...` (`.github/workflows/ci.yml`, `CLAUDE.md`
Phase 4). It runs green, `-race` included, as measured below. Nothing measures coverage. There are no
fuzz targets. `synctest` is used in three test files (`pacer`, `resolver`, `prober`), and four
service test files still call `time.Sleep`.

**Measured 2026-09-22** on `main` (Go 1.25.0 linux/amd64), running `go test -count=1
-covermode=atomic -coverprofile ./...` and then `go tool cover -func`. Each package's own tests only:

| package | stmts | coverage | notes |
|---|---:|---:|---|
| `internal/prober` | 391 | **85.7%** | SMTP state machine and classifier. `classifyPermanent` 52%, `record` 36% |
| `internal/pacer` | 223 | **84.3%** | `Acquire` wait loop, `savedPause`, `proposeBand` and `normalise` partly dark |
| `internal/limiter` | 28 | 89.3% | **the Lua script is never executed.** `fakeStore` re-implements its arithmetic in Go |
| `internal/resolver` | 132 | 90.9% | the SSRF guard. Good, but no property test |
| `internal/redis` | 118 | 84.7% | the RESP parser's error paths are dark |
| `internal/api` | 179 | **50.8%** | `bands`, `iphealth`, `suppress`, `send` handlers **0%**. Auth is proven for `/probe` and `/metrics` only |
| `internal/config` | 185 | **71.9%** | `Validate` 60.7%. Env parse errors untested |
| `internal/suppress` | 62 | 88.7% | |
| `internal/iphealth` | 178 | 88.2% | `Run` 0% |
| `internal/relay` | 404 | 74.3% | Phase C, active plans 014/015 |
| `internal/metrics` | 120 | 68.3% | |
| `internal/mxprofile` | 21 | 95.2% | |
| `cmd/verifierd` | 178 | **46.6%** | `clientAuthTLS` (the mTLS edge) 14.3%, `drain` 0% |
| **service total** (excl. `mxsim`) | 2,219 | **76.1%** | 77.0% with `-coverpkg=./...` |
| `internal/mxsim/*`, `cmd/mxsim` | — | 0–72% | test infrastructure, excluded from targets |

`go test -race -count=1 ./...` passed on the same tree.

**Defects the audit found.** Both were confirmed by throwaway tests run through `go test -overlay`,
with no file added to the repo.

1. **An out-of-range reply code is classed `invalid` (invariant 1).** `readReply`
   (`internal/prober/classify.go`) accepts any three characters `strconv.Atoi` can parse, and
   `Classify` sends everything `>= 500` to `classifyPermanent`. The results:
   `"999 no such user"` gives `invalid`, and `"600 user unknown"` gives `invalid`. (`"-55 x"` and
   `"+25 ok"` also parse, both to `unknown`.) A server has to get through banner, EHLO and
   `MAIL FROM` first, so the chance is low. The failure is still exactly the one invariant 1 names:
   a peer that does not speak SMTP condemns an address.
2. **The RESP parser panics on a hostile length.** `*1125899906842624\r\n` causes
   `makeslice: cap out of range` in `internal/redis/resp.go`. The same unchecked `make` sits behind
   `$<n>` bulk strings, and nested arrays recurse without a bound. Redis is local and trusted, so
   this is robustness rather than exposure. But a panic on a request goroutine takes down the
   process, and the fuzz target in this plan finds it in its first second.

**Gaps that are design questions, not bugs** (see Open questions in Notes):

- **`config.Validate` accepts `auth.enabled: false` with no mTLS.** `VERIFIERD_AUTH_ENABLED=false`
  with no `tls.*` loads cleanly. `run` logs a warning and serves `POST /probe` with no credentials
  at all. Invariant 11 says *every* request is authenticated.
- **The central bucket (invariant 4) has never run against Redis in a test.** The integration docs
  (`docs/03-engineering/testing/integration.md`, `ENGINEERING-STANDARDS.md` §7) say "a local Redis".
  No test uses one.

**Invariants this plan protects rather than changes.** 1 (us ≠ address), 2 (SSRF), 3 (tcp4),
4 (central bucket), 5 (fail closed), 6 (policy ≠ throttle), 7 (catch-all in two fields), 8 (no
`DATA`), 9 (suppression), 11 (authenticated edge). The only behaviour changes are the fixes for the
two defects above. Anything else a new test finds goes to `tech-debt.md` unless it breaks a hard
invariant.

## Design

**Measure the service, gate each package, and prove each invariant by property.**

1. **The coverage gate.** `scripts/coverage-gate.sh` is POSIX sh plus awk, with no new Go
   dependency (`go.mod` carries only `yaml.v3`, and it stays that way). It works like this:
   - runs `go test -count=1 -covermode=atomic -coverprofile=<tmp> ./...`, counting **each
     package's own tests only**, with no `-coverpkg`. A package is covered by its own tests, not by
     a caller that happens to walk through it
   - merges duplicate blocks and prints per-package percentages
   - fails if any package listed in `scripts/coverage-floors.txt` is below its floor, or if the
     service total is below its floor
   - excludes `internal/mxsim/*` and `cmd/mxsim` (ported test infrastructure, as in
     `.golangci.yml`)
   - **ratchets.** Floors are whole numbers and only move up. A PR that lowers one says why in the
     PR description, and the reviewer checks it.

   CI gets one new step, `coverage`, after `go test -race`. The race step stays verbatim, so the
   first item of `CLAUDE.md` Phase 4 does not change. The gate becomes a new item 5, and the PR
   checklist's "Gates" section gets the same line. The suite runs in seconds without `-race`, so a
   second run costs almost nothing. This is a deliberate choice over folding `-coverprofile` into the
   race run. Instrumentation under `-race` is slower, and keeping the two separate lets an operator
   rerun one without the other.

2. **Targets, reached before this plan closes.** These become the floors:

   | package | now | floor |
   |---|---:|---:|
   | `internal/prober` | 85.7 | **92** |
   | `internal/pacer` | 84.3 | **92** |
   | `internal/limiter` | 89.3 | **95** (and the Lua script executed, see 5) |
   | `internal/resolver` | 90.9 | **92** |
   | `internal/redis` | 84.7 | **90** |
   | `internal/api` | 50.8 | **85** |
   | `internal/config` | 71.9 | **85** |
   | `internal/suppress` / `iphealth` | 88.7 / 88.2 | **88** (hold) |
   | `internal/relay` | 74.3 | **80** |
   | `internal/metrics` | 68.3 | **80** |
   | `cmd/verifierd` | 46.6 | **60** |
   | **service total** | 76.1 | **85** |

   A percentage is a floor, not the goal. Every row in the Tasks section names the *behaviour*
   being pinned, and a test that only executes lines without asserting an invariant does not count
   toward the plan.

3. **SMTP state machine: one table, many scripted sessions.** `prober_test.go` already has a
   `net.Pipe` scripted server. It grows into a table where each row holds a server transcript, the
   options and fakes, the expected per-address `Result`, the expected `Observe(throttled)` calls,
   the tokens taken and the `ObservePolicy` calls. Rows cover every dark branch of `probeChunk`:
   IP burned, a non-guard resolve error, every IP failing to dial (and falling through to the
   second IP), no context deadline, write failure, banner EOF/timeout/`421`/`554`, EHLO and
   `MAIL FROM` network errors and `5xx`, budget running out **mid-batch** (addresses already
   answered keep their answers, the rest come back unattempted), the connection dying mid-batch,
   a policy reply feeding IP health but not the pacer, and the catch-all loop exiting early on
   budget, on connection death, or on a partial answer count. The mxsim integration tests stay for
   the multi-session behaviour (greylist window, throttle knee). A tarpit case (a server that
   stalls) runs under `synctest` where the transport allows it.

4. **Property tests and fuzzing for every parser and every invariant-shaped function.** These use
   Go native fuzzing. Seed corpora go in `testdata/fuzz/`, so **the seeds run in every
   `go test`**. That puts them in the existing gate, and the gate is still deterministic.

   | target | package | property |
   |---|---|---|
   | `FuzzReadReply` | prober | no panic. With `err == nil` the code is 200–599 (after fix 1) and the text length is at most `maxReplyBytes` + one line |
   | `FuzzClassify` | prober | **invariant 1:** `ClassInvalid` implies 500 ≤ code ≤ 599. A 4xx is never `valid`/`invalid`. `greylist` text is never `throttled`. A `senderCodes` code is never `invalid` |
   | `FuzzEnhancedCode` | prober | no panic. The output is `""` or matches `[245]\.\d{1,3}\.\d{1,3}`. `subject`/`detail` never panic |
   | `FuzzParseHint` | prober | no panic. `ok` implies `d > 0`. `clampHint` stays within [min, max] |
   | `FuzzRedactReply` | prober | **no `@` survives.** Output length ≤ limit (+ marker) |
   | `FuzzRESPReply` | redis | no panic, no allocation beyond a bound (after fix 2). Encode→decode round-trips |
   | `FuzzGuardBlocked` | resolver | **invariant 2:** any address that `netip` reports as loopback / private / link-local / unspecified / multicast, as a v4 literal or v4-mapped v6, is blocked |
   | `FuzzBandNormalise` | pacer | on JSON-decoded input, the output has 0 < `MinRate` ≤ `MaxRate`, 1 ≤ `MinConc` ≤ `MaxConc`, `Burst` ≥ 1, `Cooldown` > 0, `Pause` > 0 |
   | `FuzzMessageBuild` | relay | no header value in the output carries CR or LF (header injection) |
   | `FuzzProbeHandler` | api | any body gives a 2xx/4xx JSON response, never a panic or 5xx, and the address count never exceeds the limit |

   **Long fuzzing is not in the PR gate.** A new `fuzz` job runs on `workflow_dispatch` and on a
   weekly `schedule`, one `-fuzz=^FuzzX$ -fuzztime=60s` per target. (`-fuzz` matches one target per
   package per run, so the job loops over the list.) A crasher is minimised and committed to
   `testdata/fuzz/` as a regression seed in the same PR as its fix.

5. **The central bucket against real Redis.** A `redis_integration_test.go` in `internal/limiter`
   and `internal/pacer` runs only when `VERIFIERD_TEST_REDIS_ADDR` is set, and otherwise calls
   `t.Skip` with that variable named. CI sets the variable against a `redis:7-alpine` service
   container. The script takes the caller's clock (`ARGV[3]`, `token_bucket.lua`), so inside
   `synctest` the test sets `now` exactly. It asserts:
   - N takes over T seconds admit exactly `burst + rate·T`
   - 32 goroutines racing one key never over-admit (the one-round-trip claim)
   - burst is honoured, and refill is capped at burst
   - `rate`/`burst` ≤ 0 is refused on the Go side before the script divides by it
   - stopping the container makes `Acquire` fail closed with **zero** dials (invariant 5, the 003
     gate made permanent)

   `fakeStore` stays for unit speed. A table test runs the same sequence against both the fake and
   the real store and requires them to agree, so the fake cannot drift from the Lua.

6. **HTTP edge (invariant 11).** One route-table test builds `NewRouter` with **every** option
   non-nil and walks a hardcoded list of every route, kept in step with
   `docs/06-generated/api.md`. For each route except `/healthz` and `/readyz` it checks: no header
   gives `401` + `WWW-Authenticate`, a wrong token gives `401`, `bearer`/`Bearer  x`/`Basic` give
   `401`, and the right token is not `401`. A comment on the table says that adding a route means
   adding a row. The `0%` handlers get happy-path and error-path rows (bad JSON, over-limit import,
   unknown MX on promote, relay refusal). `clientAuthTLS` is tested with certificates generated
   in-test (`crypto/x509`, no fixtures). A real handshake through `httptest.NewUnstartedServer`
   is refused without a client certificate, refused with a certificate from a different CA, and
   accepted with one from the configured CA.

7. **Config.** A table row for every `Validate` rule, with the error checked by `errors.Is`
   against `ErrInvalid`, never by message text (ENGINEERING-STANDARDS §4). Also: env parse errors
   for duration/int/bool, the relay-enabled block, the `policy_stop`/`policy_stop_max` relation,
   the TLS pairing, and `dial_network != tcp4` as invariant 3's regression test. The auth-off
   question (Open questions) decides one more row.

8. **Race and time.** Explicit concurrency tests run under the existing `-race` gate: 64
   goroutines calling `Acquire`/`Observe` on shared and distinct hosts, concurrent `Probe` calls
   sharing the randomiser memory, and concurrent resolver cache fill and evict. The four service
   test files that still `time.Sleep` move to `synctest` where the code under test allows it
   (pr-checklist "Correctness"). `TestConcurrentSessionsAreIsolated` (tech-debt, timing-flaky)
   waits for the server's active count to reach zero instead of reading it once, and is proven
   with `-race -count=50`.

**No HTTP, Redis-key or metric change.** `06-generated/*` is untouched. The two defect fixes change
classification of inputs no conforming server sends, and change parser failure from a panic to an
error.

## Tasks

- [x] `scripts/coverage-gate.sh` + `scripts/coverage-floors.txt`, starting at today's floors
      (rounded down) so the gate is green on day one. Raise each floor as its tests land
- [x] CI: `coverage` step after `go test -race`. `redis:7-alpine` service +
      `VERIFIERD_TEST_REDIS_ADDR` on the test steps. `fuzz` job (`workflow_dispatch` + weekly
      `schedule`)
- [x] `Makefile`: `cover` and `fuzz` targets (plus `fuzz-list`, `test-redis`; `gate` now includes `cover`)
- [x] **Fix 1 (invariant 1):** `readReply` accepts only three ASCII digits with a first digit of
      2–5, and `Classify` sends codes outside 200–599 to `ClassUnknown`. Table rows for `999`, `600`,
      `-55`, `+25`, `2xx`, plus the `FuzzClassify` / `FuzzReadReply` properties
- [x] **Fix 2:** RESP bulk and array lengths are bounded, and nesting depth is bounded. An
      over-limit value is an error, not a panic. `FuzzRESPReply`
- [x] Prober session table (Design 3), covering every listed branch, with `record` asserted per
      class against a fake `Metrics`
- [x] Retry table: explicit hint in seconds/minutes/hours, clamped both ways. No hint means
      `deferralRetry`. Paused means the exact remaining time. `valid`/`invalid` carry no hint
- [x] Classifier table for the dark `classifyPermanent` branches: subjects 3/5/6 with sender-only
      wording, `5.2.2` means valid, subject 4/7 without mailbox wording means policy, and no code at
      all falls back to wording
- [x] Pacer: `Acquire` waiting on `RetryAfter` (and `≤ 0` meaning 1s) under `synctest`, and ctx
      cancelled while waiting returns `ctx.Err()` with no token. A pause persisted before a
      "restart" (a new `Pacer` on the same store) returns `PausedError` **without** calling
      `Take`. Corrupt saved rate or pause is ignored. Eviction at `MaxTracked`. No proposal at the
      ceiling. `Promote` failing on `Set`
- [x] Limiter/pacer against real Redis (Design 5), plus the fake-vs-real agreement table
- [x] Fuzz targets with seed corpora (Design 4). The seeds run in the normal gate
- [x] API route-table auth test and handler tests (Design 6). mTLS handshake tests for
      `clientAuthTLS`
- [x] Config `Validate` / env table (Design 7)
- [x] Concurrency tests. `time.Sleep` → `synctest` in `cmd/verifierd`, `pacer`, `iphealth`,
      `resolver` tests. Fix `TestConcurrentSessionsAreIsolated`
      — *done as far as the code allows:* the `pacer` and `resolver` sleeps were already inside
      `synctest`; `iphealth`'s one wall-clock sleep moved into it. `cmd/verifierd`'s sleeps are
      bounded polls against a real listener, which `synctest` cannot host (tech-debt, recorded).
      The flaky test polls with a 2s deadline. Concurrency: pacer 64 goroutines, 16 concurrent
      `Probe`s sharing randomiser memory, resolver 32-goroutine fill/evict, metrics recorders vs
      `Render` (this found defect 4, below)
- [x] `iphealth.Run`, `metrics` recorders, `drain`: one behavioural test each
- [x] Docs: `docs/03-engineering/testing/{strategy,unit,integration}.md` (coverage gate, fuzz
      targets, the Redis env var). `ENGINEERING-STANDARDS.md` §7 (fuzzing, coverage ratchet).
      `CLAUDE.md` Phase 4 + `pr-checklist.md` "Gates" (the new coverage item, matching CI).
      `tech-debt.md` (move the flaky-test item to Resolved, add anything found and not fixed).
      `changelog.md`
- [x] Tests alongside (see `docs/03-engineering/testing/`). This plan *is* the tests
- [x] `docs/06-generated/*`: no endpoint, key or metric change expected. Confirm and record it
      — *confirmed:* no route, Redis key or metric name changed. The class vocabulary in `api.md` is
      unchanged; what changed is which class some inputs get (fix 1, defect 3), recorded in the
      changelog and `patterns/smtp-classification.md`

## Definition of Done

- [x] **Manual-test gate:** `scripts/coverage-gate.sh` prints every floor in the Design 2 table met
      and a service total ≥ 85%. Recorded 2026-09-22 (no `VERIFIERD_TEST_REDIS_ADDR`; with it pacer
      reads 97.3):

      ```
      package                    stmts   cover   floor
      cmd/verifierd                178   88.2%     60%  ok
      internal/api                 179   97.7%     85%  ok
      internal/config              185  100.0%     85%  ok
      internal/iphealth            178   98.3%     88%  ok
      internal/limiter              28  100.0%     95%  ok
      internal/metrics             122   99.1%     80%  ok
      internal/mxprofile            21   95.2%     95%  ok
      internal/pacer               223   96.4%     92%  ok
      internal/prober              405   97.0%     92%  ok
      internal/redis               134   94.0%     90%  ok
      internal/relay               404   75.9%     75%  ok
      internal/resolver            132   95.4%     92%  ok
      internal/suppress             62   96.7%     88%  ok
      total                       2251   92.7%     85%  ok
      coverage-gate: ok
      ```

      **Shown failing once:** with the prober's new test files removed from the working tree
      (not a scratch branch — no branching in this session), `internal/prober 84.7% 92% BELOW FLOOR`
      and `coverage-gate: FAIL`, exit 1; files restored. Deleting a single table row does not move a
      97% package below 92, so the demonstration removes the new tests rather than one row. The same
      run caught a real gate bug: relay at 75.99% printed as `76.0%` beside `BELOW FLOOR` against a
      76 floor. The comparison and the display now both truncate, and relay's floor is 75.
- [x] Both defects are pinned. Fix 1: on the unfixed code `TestReadReplyRefusesCodesOutsideSMTP`
      failed for `999`, `600`, `-55`, `+25`, `199`, `099` and a multi-line reply ending `999`, and
      `TestClassifyOutOfRangeIsUnknown` failed with `Classify(999, "999 no such user") = invalid`,
      `Classify(600, "600 user unknown") = invalid`; both pass now. Fix 2: `*1125899906842624\r\n`
      panicked on the unfixed code (`makeslice: cap out of range`); it is now a committed seed
      (`internal/redis/testdata/fuzz/FuzzRESPReply/huge-array-length-makeslice-panic`) and returns an
      error, as do the huge-bulk and nesting-past-depth seeds.
- [x] The real-Redis tests run in CI (not skipped, visible in the job log), including 32 racing
      goroutines never over-admitting and Redis stopped → zero dials — *CI wiring done, first CI run
      pending (no push in this session).* `ci.yml` has a `redis:7-alpine` service, sets
      `VERIFIERD_TEST_REDIS_ADDR` on the test and coverage steps, and a `real-Redis tests ran` step
      that prints them and **fails if any skipped**. Verified locally against a throwaway
      `redis:7-alpine`: all limiter and pacer real-Redis tests pass under `-race` (ten repeated runs),
      including `TestRealBucketNeverOverAdmitsUnderRace` (256 takes, exactly burst 5) and
      `TestRealPacerNeverOverAdmits`. "Redis stopped → zero dials" is `TestRedisDownMeansZeroDials`,
      which needs no Redis and runs in every `go test`.
- [x] Every non-health route answers `401` without credentials in the route-table test
      (`TestEveryRouteRequiresCredentials`, 10 routes × 6 bad credential shapes), and a handshake
      without a client certificate is refused (`TestClientAuthTLSHandshake`, also wrong-CA refused,
      right-CA accepted; `TestRunServesMutualTLS` end to end through `run()`)
- [x] The weekly `fuzz` job has run once from `workflow_dispatch`, green, with each target's
      execution count recorded here — *workflow written, not dispatchable without a push.* The same
      command it runs (`make fuzz FUZZTIME=60s`) ran locally, 60s per target. First pass: FuzzClassify
      found `Classify(621, "greYlist") = deferred` in under a second (the range check sat after the
      greylist wording); fixed, committed as seed `code-621-greylist-was-deferred`, rerun. Final
      execution counts: FuzzReadReply 19,128,673 · FuzzClassify 3,396,854 · FuzzEnhancedCode
      21,400,964 · FuzzParseHint 2,015,480 · FuzzRedactReply 20,076,821 · FuzzRESPReply 26,920,054 ·
      FuzzRESPRoundTrip 7,046,128 · FuzzGuardBlocked 18,658,033 · FuzzBandNormalise 15,620,579 ·
      FuzzMessageBuild 4,103,584 · FuzzProbeHandler 2,933,295. All green.
- [x] `go test -race -count=50 ./internal/mxsim/smtp/ -run TestConcurrentSessionsAreIsolated`
      green
- [x] `go test -race -count=1 ./...` green (with and without `VERIFIERD_TEST_REDIS_ADDR`)
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean (v2.13.2, 0 issues)
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed: SSRF guard (property-tested by
      `FuzzGuardBlocked`, not changed), fail-closed (real-Redis tests plus the always-on
      zero-dials test), "us ≠ address" (fix 1, defect 3 below, the `FuzzClassify` property, and a
      per-row invariant-1 assertion in the session table). IPv4-only: `dial_network != tcp4` has
      its `Validate` rows. Central bucket: unchanged, now executed against Redis.
- [x] Docs updated per `CLAUDE.md` Phase 5. `changelog.md` entry added
- [x] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why each package's own tests, not `-coverpkg=./...`.** Cross-package coverage credits
  `internal/prober` for lines that `internal/api` tests happen to walk (api rises from 50.8% to
  57.5% that way) without asserting anything about them. The gate should say "this package's tests
  prove this package".
- **Why floors per package and not one number.** Service-wide 76% hides `internal/api` at 51% and
  the mTLS builder at 14%, which are exactly the edge invariant 11 is about. A single total can
  also be raised by testing `metrics` while `prober` goes down.
- **Why `mxsim` is excluded.** It is ported lab code kept close to upstream (plan 000,
  `.golangci.yml`). Its behaviour is what the prober integration tests assert against, and its
  flaky test is fixed here because a flaky gate is a gate people learn to rerun.
- **No new dependency.** No `miniredis` (it would bring gopher-lua and still not be the Redis that
  runs the script in production), no testify, no coverage SaaS. Real Redis in CI, the standard
  library everywhere else.
- **Deviation from "local green means CI green".** The real-Redis tests skip locally without
  `VERIFIERD_TEST_REDIS_ADDR`. The skip message names the variable and the one-line `docker run`,
  and `make test-redis` sets it. This is the one place local and CI can differ, and it is
  deliberate: the alternative is a gate that needs Docker to run at all.
- **Out of scope, recorded:** the 4xx-policy-retried-like-greylist item (`tech-debt.md`) is a
  classification decision, not a test gap. The `relay` floor is modest because 014/015 are still
  active and will reshape it.

**Open questions**

1. **Auth off without mTLS.** Should `Validate` refuse `auth.enabled: false` unless
   `tls.client_ca_file` is set or `http.addr` is loopback? Today it only warns. Recommended: yes.
   It is invariant 11 enforced rather than documented, the same move invariant 3 got. But it
   changes a boot contract, so it needs a decision before the row is written.
2. **Real Redis locally.** Is the env-gated skip acceptable, or should the test start
   `redis-server` itself when one is on `PATH`?
3. **Fuzz cadence.** Weekly × 60s per target is a guess. Should any crasher also open an issue
   automatically, or is a red scheduled run enough?
4. **Floors for `relay`.** Hold at 80 while 014/015 are active, or exclude the package until
   Phase C resumes?

**Implementation record (2026-09-22)**

- **Open questions, decided by the user's delegate for this session:** (1) auth-off stays a
  warning, not a refusal; recommended in `tech-debt.md`, pinned by
  `TestValidateAuthOffWithoutMTLSIsAcceptedToday`. (2) Real-Redis tests are env-gated;
  `make test-redis` starts a container. (3) Weekly, 60s per target, no auto-issue. (4) relay held at
  its measured value (75), not raised to 80.
- **Deviation — two more behaviour changes than the plan named, both under its own rule that a
  finding breaking a hard invariant is fixed rather than deferred, or a crash:**
  *Defect 3 (invariant 1, live).* The banner/EHLO/MAIL FROM failure paths put `Classify(code, text)`
  straight into `class`, so a pre-`RCPT` 5xx worded like a missing mailbox (Postfix
  `reject_unlisted_sender`: `550 5.1.0 … Sender address rejected: User unknown in virtual mailbox
  table`) came back `invalid` for every address in the chunk, and Data Scout's `smtp_probe.py`
  derives `accepted=False` from `class == "invalid"`. A non-220 `2xx` banner gave `class: valid`.
  `beforeRCPT` maps a would-be verdict to `policy` (5xx) or `unknown`. Found by the session table;
  five rows failed on the old code.
  *Defect 4 (crash).* `metrics.Render` read `sent` (a map), `qDepth` and `complaints` after
  releasing its lock; a scrape racing `Sent()` is a fatal concurrent map access, and `Complaint()`
  is reachable live. Found by the recorder-vs-`Render` test under `-race`.
- **Deviation — the fake bucket.** Design 5 kept `fakeStore` "for unit speed" and added an
  agreement table. The old fake ignored time and burst entirely, so it could not agree with the
  script on any sequence with refill; it is now a faithful model of the Lua and the agreement table
  holds it there.
- **Deviation — fuzz workflow placement.** A separate `.github/workflows/fuzz.yml`, not a job in
  `ci.yml`: a schedule on `ci.yml` would also run the gate and build release bundles weekly, and
  `deploy.yml` looks `ci.yml` artifacts up by commit.
- **Deviation — CI step order.** The `coverage` step sits after `golangci-lint` rather than
  immediately after `go test -race`, so CI's order matches `CLAUDE.md` Phase 4 (coverage is item 5)
  exactly; a separate `real-Redis tests ran` step follows `go test -race`.
- **Deviation — `FuzzBandNormalise` asserts `Burst > 0`, not `≥ 1`.** `normalise` only guarantees
  the former; the gap (a fractional burst never admits under the Lua) is in `tech-debt.md`.
- **Found and deferred to tech-debt (no hard invariant broken):** the fractional-burst band; a relay
  header named `From:` passing the reserved-header check (renders `From:: …`, relay not live);
  the `cmd/verifierd` and mxsim tests that poll real sockets rather than run under `synctest`.
- **Pacer tests and plan 026.** New pacer tests go through `Acquire`/`Observe`/`Proposal`/`Promote`.
  Two rows (`TestCorruptPersistedStateIsIgnored`, `TestExpiredPersistedPauseIsNotAPause`) must seed a
  corrupt or expired value and so name today's `rt:mx:<host>:*` keys, as the existing tests do;
  026 renames them with the rest.
- **New files:** `scripts/coverage-gate.sh`, `scripts/coverage-floors.txt`,
  `.github/workflows/fuzz.yml`; tests `internal/api/routes_test.go`,
  `internal/config/validate_table_test.go`, `cmd/verifierd/{tls,run_features}_test.go`,
  `internal/pacer/{pacer_more,redis_integration}_test.go`,
  `internal/limiter/redis_integration_test.go`,
  `internal/prober/{classify_fuzz,session_table,failclosed}_test.go`,
  `internal/redis/{resp_fuzz,client_errors}_test.go`, `internal/resolver/guard_fuzz_test.go`,
  `internal/metrics/recorders_test.go`, `internal/iphealth/run_test.go`,
  `internal/suppress/staleness_test.go`, `internal/relay/message_fuzz_test.go`, and seeds under
  `internal/{prober,redis}/testdata/fuzz/`.
