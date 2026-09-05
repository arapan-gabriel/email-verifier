# Plan 000 — scaffold-and-standards

**Status:** Complete (2026-08-28)
**Phase:** A
**Depends on:** none

## Goal

Stand up the Go service skeleton, tooling, and CI so every later plan lands on a green, consistent
base — the equivalent of Data Scout's plan 000.

## Context

Greenfield repo. The engine to port lives in `../ds-smtp-retry/ratecheck` (prober, pacer, redis
client, report). This plan brings up structure and gates only — no verification logic yet (that is
001).

## Design

- `cmd/verifierd/main.go` — parse config, wire a bare HTTP server, `/healthz` + `/readyz`, graceful
  shutdown on SIGTERM.
- `internal/config` — the single config source: struct loaded from env + optional YAML
  (`config/verifierd.yaml`). No hardcoded IPs/secrets (invariant 10).
- `internal/api` — router + auth middleware stub (real auth in 001), JSON error shape.
- Go module `github.com/arapan-gabriel/email-verifier`, Go 1.25.
- Tooling parity with Data Scout's rigor: `go test -race`, `go vet`, `gofmt`, `golangci-lint`.
- `.github/workflows/ci.yml` running exactly the Phase-4 gate.
- **Release artifact: one static binary, no container runtime.** `CGO_ENABLED=0 go build -trimpath
  -ldflags '-s -w'` produces a dependency-free file — that *is* the reproducible artifact. The
  deployment form is `systemd` + a Redis from the distro package (host details in 013, rationale in
  Notes below).
- `packaging/verifierd.service` — the systemd unit, kept in the repo and installed verbatim by 013.
  It carries `LimitNOFILE=65535` (thousands of concurrent SMTP sessions, ADR-001), `KillSignal=
  SIGTERM` + `TimeoutStopSec=30s` so the graceful shutdown above has time to drain, and the
  sandboxing directives (`ProtectSystem=strict`, `NoNewPrivileges=true`, `PrivateTmp=true`, empty
  `CapabilityBoundingSet`, `SystemCallFilter=@system-service`, …).
- `Makefile` — `build`, `test`, `lint`, `fmt`, `run`. The static build flags live here so the gate
  is one command locally and byte-identical in CI.
- Local dev expects a Redis on `127.0.0.1:6379`; the repo does not prescribe how it is started.
- Port `scripts/preflight.sh` from `ds-smtp-retry` unchanged.
- **Port `mxsim`** — the lab's fake MX (`../ds-smtp-retry/mxsim`, a separate module: SMTP server,
  policy engine with 421/greylist/catch-all/tarpit profiles, injectable clock, admin API, metrics).
  It lands here as `internal/mxsim/*` plus `cmd/mxsim`, so tests can drive it in-process. It belongs
  in the scaffold because plans 001, 003, 006 and 012 all name it in their Definition of Done and
  none of them tasks porting it — 001 would otherwise be blocked on unplanned work.

## Tasks

- [x] `go mod init`; commit `go.mod`/`go.sum`
- [x] `cmd/verifierd` with config load, HTTP server, healthz/readyz, graceful shutdown
- [x] `internal/config` with env+file loading and validation
- [x] `internal/api` router + JSON error middleware + auth stub
- [x] `golangci-lint` config; `gofmt`/`vet` clean
- [x] `.github/workflows/ci.yml` (test-race, vet, fmt-check, lint) + a static-build step publishing
      the binary as a CI artifact
- [x] `Makefile` (build/test/lint/fmt/run) — static build flags live here
- [x] `packaging/verifierd.service` — hardened systemd unit (`LimitNOFILE`, SIGTERM drain, sandbox)
- [x] port `scripts/preflight.sh`
- [x] port `mxsim` into `internal/mxsim/*` + `cmd/mxsim`; profiles under `config/mxsim/`
- [x] `internal/config` unit test; a healthz handler test

## Definition of Done

Verified locally and signed off 2026-08-28 (see Results below).

- [x] CI green: `go test -race -count=1 ./...`, `go vet ./...`, `gofmt -l .` empty, `golangci-lint run`
- [x] `go run ./cmd/verifierd` serves `/healthz` → 200 and shuts down cleanly on SIGTERM
- [x] `make build` produces a static binary — `ldd bin/verifierd` reports "not a dynamic executable"
- [x] `systemd-analyze verify packaging/verifierd.service` clean
- [x] `mxsim` builds and its ported tests pass in this module
- [x] `docs/06-generated/api.md` seeded with healthz/readyz
- [x] changelog entry
- [x] Status → Complete; moved to `completed/`; ROADMAP row updated

## Results (2026-08-28)

Gate, in CI order:

| Step | Result |
|---|---|
| `go test -race -count=1 ./...` | pass — `internal/api`, `internal/config`, and the ported `internal/mxsim/{admin,smtp}` suites |
| `go vet ./...` | clean |
| `gofmt -l .` | empty |
| `golangci-lint run` | clean (v2.13.2) |

Behaviour:

| Check | Result |
|---|---|
| `GET /healthz` | `200 {"status":"ok"}` |
| `GET /readyz`, store unreachable | `503 {"status":"not ready","reason":"dial unix …: no such file or directory"}` |
| `GET /readyz`, store reachable | `200 {"status":"ready"}` |
| `GET /nope` | `404 {"error":{"code":"not_found","message":"no such endpoint"}}` |
| SIGTERM | drains and exits 0; logs `stopped cleanly` |
| Bad config (`VERIFIERD_LOG_LEVEL=verbose`) | refuses to boot, exit 1, names the offending key |
| `make build` | `statically linked`; `ldd` → `not a dynamic executable`; 6.2 MB |
| `systemd-analyze verify` on the real host | only remark is the not-yet-installed `/usr/local/bin/verifierd`; `redis-server.service` resolves and every sandbox directive is accepted |
| Logs | structured JSON via `log/slog`, warning emitted while auth is disabled |

**Coding patterns adopted here and binding from now on** (`ENGINEERING-STANDARDS.md` §§2, 4, 5, 7),
retrofitted into this plan's own code rather than only written down:

- `main` is a wrapper around `run(ctx, args, getenv, stderr) error`; startup, drain and bad-config
  paths are covered by `cmd/verifierd/main_test.go` without spawning a process.
- `config.Load` takes `getenv` instead of reading the process environment, so tests assert real
  precedence without `t.Setenv`.
- `config.Validate` reports every problem at once via `errors.Join`, and every failure wraps the
  `config.ErrInvalid` sentinel so callers use `errors.Is`, not message matching. This is the small
  version of the rule that plan 001's classifier depends on absolutely — invariant 1 is carried by
  error *types*, and a string match would let a reworded provider reply delete a live mailbox.
- Interfaces are declared by the consumer: `api.ReadinessFunc` is owned by the router, implemented
  by `RedisReachable` in production and by a closure in tests.
- `testing/synctest` (GA in Go 1.25) is verified to work in this module and is now the required way
  to test elapsed time. The lab's hand-rolled `Clock` is deliberately **not** carried into the
  service; `internal/mxsim` keeps its own only because it is ported code.

One deviation worth noting: `ExecStartPre` for the preflight gate is present but commented out in
`packaging/verifierd.service`. The script it points at is installed by plan 013, and an unresolvable
`ExecStartPre` would fail both `systemd-analyze verify` here and the service start there.

## Notes / decisions / deviations

Auth is a stub here on purpose — real mTLS/API-key lands in 001 with the first real endpoint. Keep
the HTTP surface minimal until then.

**No container runtime — ADR-005**, replacing this plan's original `Dockerfile` +
`docker-compose.yml` tasks. Three project-specific reasons, not a general preference:

1. **`source_ip` must be the real egress address.** It travels with every verdict and is the basis
   of the Data Scout contract ("a verdict is only as good as the IP that produced it"). Docker's
   default bridge does SNAT, so the process would see `172.17.x.x` — the one value that must never
   be guessed would have to be hand-configured and could silently drift from reality.
2. **`172.16/12` is exactly what the SSRF guard rejects** (invariant 2). The container would sit
   inside the range it is programmed to refuse — avoidable confusion on the safety-critical path.
3. **The token bucket is on the hot path.** Take+refill runs on every probe; a bridge hop instead of
   a loopback/unix-socket hop is strictly worse and adds a failure mode to a path that is
   fail-closed by invariant 5.

There is nothing to isolate: a `CGO_ENABLED=0` Go binary has no dependencies, and the deployment is
two processes on one single-purpose host. `systemd` sandboxing supplies the hardening a container
would have provided. If a future multi-node step (ADR-004) wants images, the static binary is
already identical by construction, so a registry can be added then without touching the engine.

Full reasoning, consequences and rejected alternatives: `docs/02-architecture/decisions/
ADR-005-systemd-not-containers.md`.
