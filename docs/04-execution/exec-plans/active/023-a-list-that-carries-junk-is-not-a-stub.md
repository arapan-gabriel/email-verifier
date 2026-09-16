# Plan 023 — A list that carries junk is not a stub: the clean point gets a second opinion

**Status:** Active — started 2026-09-16
**Phase:** B
**Depends on:** 010, 021

## Goal

Let a zone that really lists RFC 5782's clean point be watched, while a resolver that answers
everything is still refused — so `rbl.your-server.de`, the list that stopped ladder day 5, can be
added to `VERIFIERD_IP_HEALTH_ZONES` at all.

## Context

`selfTestZone` (plan 021) asserts the RFC 5782 pair before a zone is trusted: `2.0.0.127` must come
back listed, `1.0.0.127` must not. The second assertion exists for one failure — **a resolver that
answers everything** — because a stub reading as "listed on every zone" would stand the node down
for a DNS misconfiguration.

Measured 2026-09-16, from the node's own resolver (`127.0.0.1:53`) and from an unrelated one, while
adding Hetzner's zone for Data Scout's plan `083`:

| query | A | TXT |
|---|---|---|
| `2.0.0.127.rbl.your-server.de` | `127.0.0.2` | `"Local RBL"` — a static test point |
| `1.0.0.127.rbl.your-server.de` | `127.0.0.2` | `"Last seen 2026-09-16 08:30:03"` |
| `1.2.0.192.rbl.your-server.de` | — | — |
| `5.5.5.5.rbl.your-server.de` | — | — |

The zone answers nothing for two unrelated addresses, so it is **not** a stub. It lists `127.0.0.1`
because its entries are auto-populated from what receiving servers report, and somebody's
misconfigured host reported its own loopback; the entry was refreshed the morning it was found, so
waiting it out is not a plan.

So the assertion fires correctly and concludes the wrong thing: *"the clean point is listed"* and
*"the resolver answers everything"* are different facts, and today the first is read as the second.
The cost is concrete — the one list that has actually refused this IP in production cannot be
watched, and plan `022` left that task open because of it.

Invariants: this is the self-test, not a verdict path, so 1 and 6 are untouched; 2 is not in scope
(no socket opens here). The package's own rule — *a false positive here is a self-inflicted outage*
— is what the change has to keep, and the fix is chosen to keep it.

## Design

`selfTestZone` gains a second opinion, used **only** when the RFC 5782 clean point comes back listed:

- Query two **unlistable points** — RFC 5737 documentation addresses `192.0.2.1` and `203.0.113.1`,
  as `1.2.0.192.<zone>` and `1.113.0.203.<zone>`. They are not routable, so no mail can have come
  from them and no honest list can carry them.
- **Both listed** → the resolver answers everything. Drop the zone, as before.
- **At least one clean** → the zone is a real list that carries junk. Keep it, and report a caveat
  the caller logs.
- **Neither answers cleanly and at least one errored** → unreachable. Drop it: the second opinion
  could not be taken, and a zone whose answers we cannot read is exactly what this test refuses.

Two points rather than one, because the finding *is* that a real list can carry an address it should
not: one bogus entry in a documentation range would otherwise disqualify a working zone the same way
`127.0.0.1` does now. A stub answers both.

Fixed addresses rather than random ones: `gosec` flags `math/rand`, a crypto source for a DNS label
is theatre, and a deterministic query is one a person can repeat by hand from the same journal line.

`SelfTest`'s return becomes a `SelfTestResult` — `Dropped` and `Caveats`, both `[]ZoneFinding`
(`DroppedZone` renamed; the type always was "a zone and what we found about it"). `cmd/verifierd`
logs the first at `error` as before and the second at `warn`.

No new endpoint, Redis key or metric. `ip_health_listed` is unchanged: a kept zone carries its series
exactly as it did.

## Tasks

- [x] `selfTestZone` takes the second opinion; `ZoneFinding` + `SelfTestResult`
- [x] `cmd/verifierd` logs a kept-with-caveat zone
- [x] Tests: Hetzner's real shape is kept with a caveat; a wildcard resolver is still dropped; a
      refusing resolver is still dropped; the unlistable points are queried only when the clean point
      is listed (two lookups, unchanged, for a zone in normal service); one junk entry in a
      documentation range does not cost the zone; an unreachable second opinion does; a keyed zone's
      caveat carries no key
- [x] Docs: `ip-reputation.md`, `RUNBOOK.md`, `metrics.md`, `tech-debt.md`, `changelog.md`, `ROADMAP.md`
- [ ] Deploy through `deploy.yml`
- [ ] `rbl.your-server.de` appended to `VERIFIERD_IP_HEALTH_ZONES` on the node, one restart

## Definition of Done

- [ ] **Manual gate:** after the deploy and the env change, `blocklist checking enabled` names four
      zones including `rbl.your-server.de`, `blocklist zone kept with a caveat` names it once, no
      `zone dropped`, and `/admin/ip-health` stays `{"burned": false}`
- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean (0 issues)
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Why not simply drop the clean-point assertion.** It is the only thing standing between a
  wildcard resolver and a platform-wide pause. The test point alone does not catch one: a wildcard
  answers `127.0.0.2` to it too, and passes.
- **Why not exempt the zone by name.** A named exception is a second list of zones to keep current,
  and the defect is general — any auto-populated list can carry a loopback entry tomorrow.
- **A kept zone with a caveat is still trusted for listings.** That is deliberate: the caveat says
  the list carries junk, not that its answer about *our* address is unreliable. If that ever stops
  being true, the answer is to stop watching the zone, not to half-trust it.
