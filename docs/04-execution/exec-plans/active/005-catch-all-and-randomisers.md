# Plan 005 — catch-all and randomisers

**Status:** Active
**Phase:** A
**Depends on:** 001, 003

> Renamed from `005-catch-all-and-classification`. Two of the original four design points are gone:
> the **per-domain catch-all cache** is Data Scout's, which already has
> `email_domain_profile_service.knows_catch_all` and uses it to decide `need_catch_all` before it
> calls here; and the **status reconciliation table** is moot under ADR-006, where this service
> returns facts and Data Scout does the scoring.

## Goal

Tell a genuine catch-all apart from a server that answers at random, and remember the second — it is
a property of the *server*, so it condemns every domain hosted there, not just the one being asked
about.

## Context

Plan 001 sends **one** bogus `RCPT` per request. That is enough to catch a plain catch-all: accepted
means every `250` from this domain is worthless. It is not enough for Microsoft-class hosts, which
answer inconsistently — one bogus probe lands on `accepted` or `rejected` more or less by coin flip,
so the same domain reports catch-all on one run and clean on the next, and a real mailbox behind it
gets reported `valid` on the strength of a `250` that meant nothing.

The distinction the lab measured, and the reason it matters:

- **Catch-all is a domain property.** `example.com` accepts everything; its neighbours on the same
  MX are unaffected.
- **A randomiser is a server property.** The host itself is unreliable, so *every* domain it serves
  is suspect — including ones we have not probed yet.

Getting this backwards reports a nonexistent mailbox as `valid`, which is the failure mode this
whole service exists to avoid.

## Design

- **N bogus local parts, not one** (`probe.catch_all_probes`, default 3). All accepted → catch-all.
  All rejected → the real answers can be trusted. **Mixed → randomiser.**
- Each bogus probe costs a token like any other recipient (plan 003) — asking three questions must
  cost three questions' worth of budget.
- **`internal/mxprofile`** remembers a randomiser verdict per MX host in Redis, `mx:<host>:randomiser`
  with a TTL. On a later request for *any* domain on that host, the probes are skipped and the
  results carry the verdict — that is what "condemns every domain on that host" means in practice.
- The response gains `randomiser`. A randomiser host also sets `catch_all: true`: it is the
  conservative reading, and it is the field existing consumers already handle correctly. Callers
  that understand `randomiser` get the precise reason; callers that do not still refuse to trust the
  `250`.
- A store failure is not fatal here. Unlike the rate budget, not knowing whether a host randomises
  costs accuracy, not safety — so it degrades to "probe again" rather than failing the request.

## Tasks

- [x] N bogus probes with the catch-all / randomiser / clean decision
- [x] `internal/mxprofile` — per-server randomiser verdict in Redis with TTL
- [x] Known randomiser short-circuits the probes and marks the results
- [x] `randomiser` in the response and in `06-generated/api.md`
- [x] `probe.catch_all_probes` in config, validated
- [x] Tests: catch-all, clean, randomiser, randomiser remembered across domains, budget accounting
- [x] Update `features/002-catch-all-detection.md`, `redis-contract.md`, `smtp-classification.md`,
      changelog

## Definition of Done

- [x] A catch-all `mxsim` profile reports `catch_all:true`, `randomiser:false`
- [x] A normal profile reports `catch_all:false` and a real mailbox `accepted:true`
- [x] A host answering inconsistently reports `randomiser:true` **and** `catch_all:true`
- [x] A second request, for a **different domain** on a known-randomiser host, carries the verdict
      without re-probing
- [x] The bogus probes are counted against the rate budget
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated — pending manual sign-off

## Results (2026-08-28)

Gate clean.

| Check | Result |
|---|---|
| `mxsim` catch-all profile | `catch_all:true`, `randomiser:false` |
| `mxsim` gmail profile | `catch_all:false`; real mailbox `accepted:true`, missing one `550 5.1.1` |
| Host accepting every other bogus probe | `randomiser:true` **and** `catch_all:true`, verdict remembered against the host |
| Second request, different domain, same host | carries `randomiser:true` with **zero** bogus probes sent |
| Budget | 2 recipients + 3 catch-all probes = 5 tokens |
| Store failure | degrades to probing again, not to a failed request |
| `probe.catch_all_probes < 2` | refuses to boot |

## Notes / decisions / deviations

**`mxsim` has no randomising profile.** Its chaos knobs produce random *4xx deferrals*, not random
accept/reject, so the coin-flip host is exercised with a scripted dialer instead. Adding an
`accept_rate` knob to the ported simulator would let the integration suite cover it too — worth
doing if plan 012 needs to calibrate against one, not worth changing ported code for on its own.


Three probes is a compromise, not a measurement: it is enough to catch an obviously inconsistent
host cheaply. The lab kept up to 30 available for Microsoft-class servers, and raising the count for
a host already suspected of randomising is calibration work — plan 012 — not something to spend on
every request.
