# Plan 020 — turn-on-ip-health-and-suppression

**Status:** Complete (2026-09-11)
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

- [x] Node: `unbound` on `127.0.0.1:53`, localhost-only, IPv6 off; `systemd-resolved` keeps
      `.53`/`.54` and `/etc/resolv.conf` is unchanged — verified before and after
- [x] Node: `ip_health.resolvers: ["127.0.0.1:53"]`; the startup warning is gone and the log now
      reads `blocklist checking enabled`
- [x] **The checker has been seen to report a listing.** `blocklist checking enabled` is printed
      *only* when `SelfTest` returns nil, and `SelfTest` requires every zone to report its own
      documented test point as **listed** — so the enable path is that assertion. Deliberately not
      demonstrated by pointing the service's `source_ip` at a listed address: that burns a node
      serving a live warm-up to show what the startup gate already shows
- [x] The node's own IP against every zone — recorded in Results
- [x] `VERIFIERD_SUPPRESS_SALT` on the node, the same value as Data Scout's secret
- [x] Data Scout: `app/services/suppression_export.py` + `app/tasks/suppression_tasks.py` —
      hourly by beat, and a direct dispatch from both purge endpoints; always `mode: replace`;
      digests only. `deploy.yml` carries the salt
- [x] Node: `suppress.enabled: true`, after the first real export landed — not before
- [x] A digest that is present refuses a probe before a socket opens — verified live
- [x] Update `operations/ip-reputation.md`, `SECURITY.md`, `redis-contract.md` if the import shape
      changes, changelog in both repositories
- [x] ROADMAP: 010 and 011 rows say *live*, not merely *done*

## Definition of Done

- [x] Startup logs neither "blocklist checking is off" nor "local suppression check is off"
- [x] `SelfTest` passes, and a deliberately listed address is reported **listed** — the check has
      been seen to fire, not only to stay quiet
- [x] The node's own IP is confirmed against every configured zone, recorded with the date
- [x] Redis holds a suppression list with a version from a real export, and `Status()` reports it
      fresh
- [x] A suppressed digest is refused **before any socket opens**, shown live
- [x] A stale list is loud on the verify path and does not silently pass — asserted
- [x] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [x] Docs updated per `CLAUDE.md` Phase 5; changelog entries in both repositories
- [x] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` rows updated

## Results (2026-09-11)

### 010 — live

`unbound` on `127.0.0.1:53`, localhost-only. Two packages the `--no-install-recommends` install had
stripped were needed first: without `unbound-anchor` and `dns-root-data` the service failed at
startup on an empty DNSSEC root key, loudly and harmlessly — the host's own resolution never moved.

**The measurement that justifies the whole exercise**, same query, two resolvers:

| query | via `1.1.1.1` | via `127.0.0.1` |
|---|---|---|
| `2.0.0.127.zen.spamhaus.org` (documented **test point**) | `127.255.255.254` | `127.0.0.10` |
| `2.0.0.127.bl.spamcop.net` | `127.0.0.2` | `127.0.0.2` |
| `1.0.0.127.zen.spamhaus.org` (documented **clean** point) | — | *(empty)* |

`127.255.255.254` is Spamhaus's "query refused, open resolver" sentinel, not a listing. So a service
pointed at a public resolver would have answered *not listed* to every question forever, including
about an address that was listed. `iphealth.query` already excludes that sentinel, and `SelfTest`
catches the resolver anyway — a refused resolver returns it for the clean point too, so the test
point never comes back listed and checking stays off.

**This node, 2026-09-11: clean on `zen.spamhaus.org`, `bl.spamcop.net` and
`b.barracudacentral.org`.** `/metrics` now carries real values —
`ip_health_listed{ip="92.222.87.97",list="zen.spamhaus.org"} 0` — and `/admin/ip-health` reports
`{"burned":false}`.

One small fix on the way: the enable log printed `zones=[]`, the *configured* list, where empty
means "the defaults". A line that says nothing about what is being checked is worse than no line;
it now logs the effective zones.

### 011 — built, and switched on only as far as it honestly can be

The export is written and tested on the Data Scout side: digests only, always `mode: replace` —
because an incremental push can add and can never take away, so an address suppressed in error would
stay suppressed on the far side forever. Hourly by beat, **and dispatched directly from both purge
endpoints**, because the window between someone asking to be forgotten and the far side honouring it
is the only time-sensitive part, and an hour of it is a probe that should not have happened. The
dispatch can never fail an erasure: the erasure is the legal obligation and the far copy is a
redundancy.

**The digest formula was checked against the other implementation rather than against its
documentation** — the mistake this pair of repositories made once already, and paid a fortnight for:

```
Python  ef4655f5465cbb97ebefb7543cb4950f8818c467978fe654a90db21744e3df0f
Go      ef4655f5465cbb97ebefb7543cb4950f8818c467978fe654a90db21744e3df0f
```

for `Hash("pepper", "  SomeOne@Example.COM  ")`, which also pins the normalisation — trimmed and
lowercased on both sides — as part of the contract rather than an accident.

**`suppress.enabled` stays `false`, deliberately.** Redis holds two hashes from a plan-011 test,
computed with a different salt. Enabling against that list would be worse than leaving it off: a
suppression check that answers "not suppressed" for everyone is indistinguishable from one that is
working. It is flipped after the first real export lands, not before — which is the same mistake
this plan exists to fix, so it is not going to be made again inside it.

### 011 — live, after the design flaw that made it impossible

The first real export returned **404**. `POST /admin/suppress` is registered only when the list
object exists, and the list object was built only when `suppress.enabled` was true — so a list could
not be loaded without first switching on enforcement, **against a list nobody had pushed**, which is
exactly what this plan said not to do. Neither side could have shown that by reading; it took trying
to use it.

The cause was one method answering two questions. `Enabled()` meant "salt and store are configured"
to `Import` and `Status`, and "refuse probes" to the prober. They are separate now: `Enabled()` is
still *configured*, `Enforcing()` is *configured and switched on*, the list is built whenever a salt
is set, and the node can finally be in the state it could not previously express —
`suppression list loadable but not enforced`.

Then, in order:

| step | result |
|---|---|
| first real export | `export-2026-09-11T12:06:19Z`, 0 entries — and the two August test hashes **gone**, `mode: replace` doing the job it exists for |
| `suppress.enabled: true` | `suppression check enforced entries=0 version=export-…` |
| probe a live address | `class=invalid`, `550` — a real session happened |
| add its digest via `POST /admin/suppress` | `200` |
| probe it again | **`class=suppressed`, `connected=false`, `accepted=null`** — no socket opened, and the address is *not* condemned |
| re-push the authoritative list | back to 0, the test digest evicted, the address probes normally again |

`accepted: null` rather than `false` is the part worth keeping: a suppressed address is one we are
forbidden to ask about, not one that failed. Conflating those would let an erasure request turn into
a deliverability verdict.

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
