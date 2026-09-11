# Plan 020 — turn-on-ip-health-and-suppression

**Status:** Planned
**Phase:** B
**Depends on:** 010, 011, 013 — all Complete, which is the problem
**Blocks:** 014 (the relay checks both before every send)

## Goal

Make two protections that are signed off as finished actually run. `ip_health` has never queried a
blocklist and `suppress` has never refused an address, on the one host whose IP reputation is the
entire premise of this project.

## Context

Plans 010 and 011 are in `completed/` and the ROADMAP shows them ✅. Both are built, tested, and
**switched off in production**:

```
ip_health.resolvers: []        # no DNSBL-capable resolver named
suppress.enabled:   false      # and Redis holds 2 test hashes, version export-2026-08-28
```

The service says so at every start, at `WARN` and `INFO` respectively, which is the honest thing to
do and also the reason nobody noticed for a fortnight. Neither design is wrong — both were
deliberately opt-in, because a blocklist check through a resolver that cannot answer would report
"clean" forever, and a suppression list without its salt would silently miss every lookup. Opt-in
was right. Nobody opted in.

**This is not tidying.** Plan 014 sends real mail and checks both before every send, and
`SECURITY.md` records that the relay must fail **closed** on suppression because sending is
irreversible. Building 014 on two switches that are off would produce a relay whose safety checks
are decorative.

And the warm-up ladder is the live argument for 010: the node is spending real addresses at real
providers, at a rate that climbs to 2,000 a day, and it would not currently notice being listed.

## Design

### 010 — a resolver that can actually answer

The major zones **refuse queries from public resolvers**, which is not a guess: `preflight.sh`
reports `zen.spamhaus.org: query refused (open/public resolver)` through `1.1.1.1` on every start.
So `ip_health.resolvers: ["1.1.1.1:53"]` would not fix this; it would produce a checker that returns
"not listed" because the answer never arrives.

Two ways to get a resolver the zones will answer:

1. **A local recursive resolver on the node.** `unbound` bound to `127.0.0.1:53` — free, and
   `systemd-resolved` holds only `127.0.0.53` and `127.0.0.54`, so the address is unused. Queries
   then leave from the node's own IP, which is what the zones' free tiers expect at this volume.
2. **A Spamhaus DQS key**, which gives a dedicated zone that answers regardless of resolver. Free
   for low volume, but it is a third-party credential to hold and rotate, and it covers Spamhaus
   alone.

**Recommended: (1).** It needs no account, covers every zone at once, and keeps the reputation
question answerable from the box that owns the reputation. *(2)* stays the fallback if the ISP or
the zones start refusing the node's own queries.

**The self-test is what makes this safe to turn on.** `Health.SelfTest` queries each zone's
documented test point (`2.0.0.127`) and refuses to enable unless it comes back *listed* — so a
resolver that cannot reach the zone fails loudly at startup instead of reporting a clean IP forever.
That check already exists; this plan is mostly about giving it something to test.

### 011 — a list that is actually the list

The salt must be set on **both** sides (`VERIFIERD_SUPPRESS_SALT` here, the same value there) or
every lookup misses silently — which is why enabling without it refuses to boot. Then Data Scout
needs a job that exports digests and `POST`s them to `/admin/suppress`: `mode: replace`, a version
string, `sha256(salt + "\x00" + value)` per entry, never an address (the endpoint refuses those).

What is there today is two hashes from a plan-011 test, labelled `export-2026-08-28`. Turning
`enabled: true` against that list would be worse than leaving it off: a suppression check that
answers "not suppressed" for everyone looks identical to one that is working.

**Cadence is a decision, not an obvious default.** Suppression is a GDPR erasure mechanism: the gap
between a person asking and this node honouring it is a window in which they can still be probed.
Data Scout checks the authoritative list three times before calling, so the window is narrow in
practice, but the number should be chosen rather than inherited — hourly is the obvious start, with
the push also triggered directly on an erasure request.

### What "on" must mean before 014

`suppress.stale` is 24h. On the verify path a stale copy is loud but not fatal — the authoritative
check has already run upstream. **The relay has no upstream check between the queue and the socket**,
so for sending, stale must mean stop. That distinction is already written in `SECURITY.md`; 014 will
implement it, and this plan is what gives it something to be stale *about*.

## Tasks

- [ ] Node: install and configure a local recursive resolver on `127.0.0.1:53`; confirm
      `systemd-resolved` is undisturbed and the host's own name resolution still works
- [ ] Node: `ip_health.resolvers: ["127.0.0.1:53"]`; confirm `SelfTest` passes at startup and the
      `blocklist checking is off` warning is gone
- [ ] **Verify the checker can see a listing, not only a clean answer** — query a zone's documented
      listed test point through the service and assert it reports listed. A checker that has only
      ever returned "clean" is untested
- [ ] Confirm the node's own IP against every configured zone, and record the answer here
- [ ] Decide and set `VERIFIERD_SUPPRESS_SALT`; the same value on the Data Scout side
- [ ] Data Scout: a scheduled export of digests to `POST /admin/suppress` (`mode: replace`,
      versioned), plus a direct push on an erasure request; **never an address**
- [ ] Node: `suppress.enabled: true` once a real list is present, not before
- [ ] A digest that is present refuses a probe before a socket opens (invariant 9) — verified live
- [ ] Update `operations/ip-reputation.md`, `SECURITY.md`, `redis-contract.md` if the import shape
      changes, changelog in both repositories
- [ ] ROADMAP: 010 and 011 rows say *live*, not merely *done*

## Definition of Done

- [ ] Startup logs neither "blocklist checking is off" nor "local suppression check is off"
- [ ] `SelfTest` passes, and a deliberately listed address is reported **listed** — the check has
      been seen to fire, not only to stay quiet
- [ ] The node's own IP is confirmed against every configured zone, recorded with the date
- [ ] Redis holds a suppression list with a version from a real export, and `Status()` reports it
      fresh
- [ ] A suppressed digest is refused **before any socket opens**, shown live
- [ ] A stale list is loud on the verify path and does not silently pass — asserted
- [ ] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [ ] Docs updated per `CLAUDE.md` Phase 5; changelog entries in both repositories
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` rows updated

## Notes / decisions / deviations

**Why this is a plan and not a checklist item inside 014.** Two completed plans describe protections
that have never run. That is a gap in how they were closed — their designs made the features opt-in
and their sign-offs did not ask whether anyone had opted in — and folding the fix into a Phase C
plan would bury it again. It also lets 010 and 011 be *live* before the relay needs them rather than
because the relay needs them.

**The lesson worth carrying.** A plan whose feature ships disabled should not be signed off until
either it is enabled or the sign-off says, in the DoD, that it is deliberately dark and who will
turn it on. Both of these said so in their bodies and neither said it in a task anyone would trip
over. Worth applying to 014 and 015, which will both have switches of their own.
