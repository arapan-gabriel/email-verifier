# Testing — strategy

- **Unit** (default): table-driven, no sockets. The SMTP transport and Redis are behind interfaces
  so tests are deterministic (mirror Data Scout's `set_prober` seam).
- **Integration**: the engine against a fake SMTP server (port the `ds-smtp-retry` `mxsim`
  simulator: 421 throttling, greylisting, catch-all, tarpits, injectable clock) and a real/local
  Redis. Assert on what the *server* saw, not just the client's verdict.
- **Time-dependent behaviour uses `testing/synctest`** (Go 1.25). Cooldowns, greylist backoff,
  bucket refill windows and AIMD recovery are asserted on a fake clock in microseconds, not slept
  through. Do not port the lab's hand-rolled `Clock` into the service — see
  `ENGINEERING-STANDARDS.md` §7. The exception is a test against a real socket (a TCP listener, a
  real Redis): synctest cannot advance past network I/O, so those poll with a deadline instead.
- **Property tests and fuzzing** for every parser and every invariant-shaped function (plan 027).
  Go native fuzzing; see the target list below. Each target's `f.Add` seeds and any file under its
  package's `testdata/fuzz/<Target>/` run in every plain `go test`, so they are part of the gate and
  the gate stays deterministic. Long fuzzing is a separate weekly job (`.github/workflows/fuzz.yml`,
  `make fuzz`), 60s per target; a red run is the alert. A crasher is minimised, committed to
  `testdata/fuzz/<Target>/` as a regression seed, and fixed in the same PR.
- **Coverage is gated per package, and ratchets** (plan 027). `scripts/coverage-gate.sh` counts
  each package by its own tests only (no `-coverpkg`) and fails if any package in
  `scripts/coverage-floors.txt`, or the service total, is below its floor. `internal/mxsim/*` and
  `cmd/mxsim` are excluded. Floors are whole numbers and only move up; a PR that lowers one says why.
  A percentage is a floor, not the goal: a test that executes lines without asserting a behaviour
  does not count.
- The gate matches CI exactly: `go test -race -count=1 ./...`, then `scripts/coverage-gate.sh`
  (`make gate` runs the lot).
- Mandatory regression tests for every invariant: us≠address, SSRF refusal, fail-closed,
  policy≠throttle.

## Fuzz targets

| target | package | property |
|---|---|---|
| `FuzzReadReply` | prober | no panic; an accepted reply's code is 200–599; kept text bounded by `maxReplyBytes` plus one line |
| `FuzzClassify` | prober | **invariant 1**: `invalid` only for 500–599; a 4xx is never `valid`/`invalid`; greylist wording is never `throttled`; a sender-only code is never `invalid`; outside 200–599 is `unknown` |
| `FuzzEnhancedCode` | prober | no panic; the output is `""` or `[245]\.\d{1,3}\.\d{1,3}` |
| `FuzzParseHint` | prober | a parsed hint is positive; `clampHint` stays in [min, max] |
| `FuzzRedactReply` | prober | **no `@` survives**; output within the rune limit plus the marker |
| `FuzzRESPReply` | redis | no panic on any input (hostile lengths are errors) |
| `FuzzRESPRoundTrip` | redis | encode → decode round-trips any argument list |
| `FuzzGuardBlocked` | resolver | **invariant 2**: anything `netip` calls loopback/private/link-local/unspecified/multicast is blocked, as v4, v4-mapped v6, or v6 — and `Resolve` refuses it as a literal |
| `FuzzBandNormalise` | pacer | a normalised band has 0 < min ≤ max rate, 1 ≤ min ≤ max concurrency, burst > 0, positive cooldown and pause |
| `FuzzMessageBuild` | relay | no header name or value carries CR/LF; the rendered header block has one line per header |
| `FuzzProbeHandler` | api | any body gives JSON with a 2xx/4xx, never a panic or 5xx; the engine never sees more addresses than the limit |

The list lives in the `Makefile` (`FUZZ_TARGETS`, `make fuzz-list`); keep this table in step.
