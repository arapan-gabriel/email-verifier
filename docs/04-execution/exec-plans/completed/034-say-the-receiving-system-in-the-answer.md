# Plan 034 — Say the receiving system in the answer

**Status:** Complete (2026-10-02)
**Phase:** B
**Depends on:** 026 (pace keys), 033 (several domains per session)

## Goal

Every `POST /probe` reply names the receiving system the request's MX belongs to (`pace_key`) and
whether that system may, on this node and right now, be asked about several domains in one session
(`multi_domain`). Data Scout's plan 114 groups a verify run by that key instead of by exact MX host,
without carrying a copy of `families.json`.

## Context

- Plan 033 serves a `domains: [...]` request as one session for a family cleared in `families.json`
  (`multi_domain: true` — only `@google` today) and falls back to one session per domain for every
  other family, or for a cleared one after a receiver refused a foreign domain (`fallen`, until
  restart).
- Data Scout's coalescer can only group what it can name. Today it groups by **exact MX host**, so
  `aspmx.l.google.com` and `smtp.google.com` — one system here — queue apart. Job 801 (160 Google
  domains) took 68 sessions where ~32-40 were possible; Data Scout plan 114 has the measurement.
- Copying the family table into Data Scout was rejected there (two tables meeting only in
  production); a `GET /families` endpoint was rejected (a new authenticated surface, invariant 11,
  and it would not carry a runtime fallback). The reply already exists and is already authenticated.

## Design

- `prober.Response` gains `PaceKey string` and `MultiDomain bool`, set by `Prober.Probe` **after**
  the session from `p.grouping(req.MXHost)` — so a fallback that happened *in this request* is
  already reflected (`multi_domain: false`). Both shapes (single domain and `domains`) set them.
- `POST /probe` 200 gains `"pace_key"` (omitted when the prober has no family function, i.e. tests)
  and `"multi_domain"`. Additive: Data Scout before plan 114 does not read them; the request schema
  is unchanged (`DisallowUnknownFields` is about requests only).
- `pace_key` is the same value the bucket, the band and the concurrency lease use (invariant 4), so
  what Data Scout groups by is exactly what the node paces by. For an unlisted host it is the host.
- No Redis key, no metric, no config.

## Tasks

- [x] `internal/prober`: `Response.PaceKey`, `Response.MultiDomain`; `Probe` sets them after the work
- [x] `internal/api/probe.go`: `pace_key`, `multi_domain` in the 200
- [x] Tests: a listed multi-domain host → its key + true; a listed non-multi host → key + false; an
      unlisted host → the host + false; after a fallback → false; the handler serialises both
- [x] `docs/06-generated/api.md`: the two fields and what a caller may do with them
- [x] Deploy; `POST /probe` for `aspmx.l.google.com` answers `pace_key: "@google", multi_domain: true` — deployed `83ca941` (run 37054159295, 19:27 UTC); Data Scout's `verify.probe_batch` logged `pace_key=@google multi_domain=True` for both `aspmx.l.google.com` and `smtp.google.com`

## Definition of Done

- [x] The manual-test gate passes: a reply from the deployed node carries both fields (above)
- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed — no socket, guard, budget or
      verdict path changes; the fields describe the node's pacing, never an address
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [x] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Gate (2026-10-02, with Data Scout plan 114 deployed):** job 801's 160 Google Workspace addresses
  through Data Scout's engine — **22 `session_leased`** (was 68), all `@google`; 160 `rcpt_paced`; zero
  `451 4.3.0`, zero "multiple destination", zero `multi_domain_fallback`; 13 requests carried domains of
  both Google hosts; 160/160 verdicts identical to job 801's. `make gate` green before deploy (prober
  95.1%); the real-Redis tests were left to CI (no Redis path changed) — CI green on `83ca941`.

- **Why after the session, not before.** A fallback is decided mid-session (a receiver refusing a
  foreign domain). Read before, the reply that *caused* the fallback would still say `true`, and Data
  Scout would form one more cross-host group before learning otherwise.
