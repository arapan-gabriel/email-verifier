# Plan 000 — scaffold-and-standards

**Status:** Planned
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
- Go module `github.com/<org>/email-verifier`, Go 1.25.
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

## Tasks

- [ ] `go mod init`; commit `go.mod`/`go.sum`
- [ ] `cmd/verifierd` with config load, HTTP server, healthz/readyz, graceful shutdown
- [ ] `internal/config` with env+file loading and validation
- [ ] `internal/api` router + JSON error middleware + auth stub
- [ ] `golangci-lint` config; `gofmt`/`vet` clean
- [ ] `.github/workflows/ci.yml` (test-race, vet, fmt-check, lint) + a static-build step publishing
      the binary as a CI artifact
- [ ] `Makefile` (build/test/lint/fmt/run) — static build flags live here
- [ ] `packaging/verifierd.service` — hardened systemd unit (`LimitNOFILE`, SIGTERM drain, sandbox)
- [ ] port `scripts/preflight.sh`
- [ ] `internal/config` unit test; a healthz handler test

## Definition of Done

- [ ] CI green: `go test -race -count=1 ./...`, `go vet ./...`, `gofmt -l .` empty, `golangci-lint run`
- [ ] `go run ./cmd/verifierd` serves `/healthz` → 200 and shuts down cleanly on SIGTERM
- [ ] `make build` produces a static binary — `ldd bin/verifierd` reports "not a dynamic executable"
- [ ] `systemd-analyze verify packaging/verifierd.service` clean
- [ ] `docs/06-generated/api.md` seeded with healthz/readyz
- [ ] changelog entry; Status → Complete; moved to `completed/`; ROADMAP row updated

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
