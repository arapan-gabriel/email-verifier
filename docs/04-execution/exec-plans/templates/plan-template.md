# Plan NNN — <name>

**Status:** Planned | Active | Complete (<sign-off date>)
**Phase:** A | B | C
**Depends on:** <plan numbers, or "none">

## Goal

One or two sentences: what this plan delivers and why it is next.

## Context

What exists now, what is being changed, and the relevant invariants
(`CLAUDE.md`) and architecture (`docs/02-architecture/ARCHITECTURE.md`) this
must respect. Link the ported source in `ds-smtp-retry` where applicable.

## Design

The approach. Key types/packages touched (`internal/...`). Redis keys added or
used (must match `docs/06-generated/redis-contract.md`). HTTP shape added (must
match `docs/06-generated/api.md`). Call out anything that deviates from the lab
and why.

## Tasks

- [ ] ...
- [ ] Tests alongside (see `docs/03-engineering/testing/`)
- [ ] Update `docs/06-generated/*` for any endpoint / key / metric change

## Definition of Done

- [ ] The manual-test gate from `ROADMAP.md` passes, recorded here with how it was verified
- [ ] `go test -race -count=1 ./...` green
- [ ] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed (SSRF guard, fail-closed,
      "us ≠ address" where relevant)
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

Anything a future reader needs: trade-offs made, things deferred to tech-debt,
library choices.
