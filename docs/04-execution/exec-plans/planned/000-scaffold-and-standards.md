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
  (`config/verifierd.yaml`). No hardcoded IPs/secrets (invariant 8).
- `internal/api` — router + auth middleware stub (real auth in 001), JSON error shape.
- Go module `github.com/<org>/email-verifier`, Go 1.25.
- Tooling parity with Data Scout's rigor: `go test -race`, `go vet`, `gofmt`, `golangci-lint`.
- `.github/workflows/ci.yml` running exactly the Phase-4 gate.
- `Dockerfile` (distroless static) + `docker-compose.yml` (verifierd + redis) for local.
- Port `scripts/preflight.sh` from `ds-smtp-retry` unchanged.

## Tasks

- [ ] `go mod init`; commit `go.mod`/`go.sum`
- [ ] `cmd/verifierd` with config load, HTTP server, healthz/readyz, graceful shutdown
- [ ] `internal/config` with env+file loading and validation
- [ ] `internal/api` router + JSON error middleware + auth stub
- [ ] `golangci-lint` config; `gofmt`/`vet` clean
- [ ] `.github/workflows/ci.yml` (test-race, vet, fmt-check, lint)
- [ ] `Dockerfile` + `docker-compose.yml` (verifierd + redis)
- [ ] port `scripts/preflight.sh`
- [ ] `internal/config` unit test; a healthz handler test

## Definition of Done

- [ ] CI green: `go test -race -count=1 ./...`, `go vet ./...`, `gofmt -l .` empty, `golangci-lint run`
- [ ] `go run ./cmd/verifierd` serves `/healthz` → 200 and shuts down cleanly on SIGTERM
- [ ] `docker compose up` brings up verifierd + redis
- [ ] `docs/06-generated/api.md` seeded with healthz/readyz
- [ ] changelog entry; Status → Complete; moved to `completed/`; ROADMAP row updated

## Notes / decisions / deviations

Auth is a stub here on purpose — real mTLS/API-key lands in 001 with the first real endpoint. Keep
the HTTP surface minimal until then.
