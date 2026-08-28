# Plan 010 — ip-health-and-blocklists

**Status:** Active
**Phase:** B
**Depends on:** 003, 009

## Goal

Notice when the sending IP is burned — blocklisted or being policy-refused — and pause its traffic
automatically, before it does more damage. The IP is the asset; this protects it.

## Context

A `RCPT`-heavy IP gets listed in hours (build-vs-buy §3.4). The lab already treats `ClassPolicy`
(`5.7.x` / `554 blocked`) as an IP signal, not a rate one. This plan turns that into a health state
and an operational response. Component: `operations/ip-reputation.md`.

## The risk that shapes this plan

Automatically pausing the node is the point, and it is also the danger: **a false positive here is a
self-inflicted outage.** Three ways to get one, all of them measured on our own IP rather than
imagined:

1. **A DNSBL query through the wrong resolver answers "listed" for everything.** The RUNBOOK says
   so, and the deployed node is exactly that case — `systemd-resolved` on `127.0.0.53`. A naive
   check there would report every list positive and pause the node permanently.
2. **UCEPROTECT L3 lists the whole ASN.** Measured on 2026-08-28: our IP is on it because AS16276 is,
   while Spamhaus, SpamCop and UCEPROTECT L1/L2 are clean and *both Gmail and Microsoft accepted the
   session*. No delisting we can perform clears it. Treating it as "burned" would pause forever for
   a condition we cannot fix and that no provider we tested enforces.
3. **One server refusing us is not the IP being burned.** A rising `ClassPolicy` rate from a single
   MX host is that host's opinion — plan 007 already stops probing it. It only says something about
   the IP when it spreads across *several distinct hosts*.

## Design

- `internal/iphealth` holds per-IP state in Redis `ip:health:<ip>`, from two independent signals.
- **DNSBL checks are opt-in and self-testing.** No resolver configured for them means no checking at
  all, said plainly at startup — never a silent check through a stub. Before the first real query,
  the resolver is probed against the list's documented test points (`2.0.0.127` must come back
  listed, `1.0.0.127` must not). A resolver that fails that is not trusted, and DNSBL checking
  disables itself with an alert rather than pausing anything.
- **Only actionable lists count.** Zones are configured, default Spamhaus ZEN and SpamCop.
  **UCEPROTECT L3 is excluded by default and documented as such**, with the measurement above as the
  reason.
- **Confidence decides consequence.**
  - A confirmed listing on an actionable list → **pause the node**. High confidence, and the damage
    of continuing is worse than the damage of stopping.
  - A policy rate above threshold across several distinct MX hosts → **alert only**. It is a real
    signal but a noisier one, and pausing on it hands any hostile or misconfigured MX a way to take
    the node down.
- **A pause is reversible without a redeploy** — an operator endpoint clears it, and the next check
  re-evaluates.
- A probe refused because the node is burned is `class:ip_burned`, `connected:false`,
  `accepted:null` — our refusal, never a verdict (invariant 1).
- A policy block still never drives per-MX AIMD (invariant 6). It feeds this, and nothing else.
- Health is per-IP, so with more than one node a burned one stands itself down without touching the
  others (ADR-004).

## Tasks

- [x] `internal/iphealth`: DNSBL check over configured zones, state in `ip:health:<ip>`
- [x] **Resolver self-test against the lists' documented test points**; failure disables checking
      and alerts, and never pauses
- [x] Policy-rate signal counted across *distinct* MX hosts; alert-only
- [x] Burned → probes refused with `class:ip_burned`; a manual resume that needs no redeploy
- [x] `ip_health_listed{ip,list}` in `internal/metrics`
- [x] Background checker on a ticker, stopped with the server
- [x] Tests: listing pauses, self-test failure does not, one noisy host does not, resume works
- [x] Update `redis-contract.md`, `metrics.md`, `api.md`, `operations/ip-reputation.md`, changelog

## Definition of Done

- [x] A simulated listing on an actionable zone flips health and refuses probes with
      `class:ip_burned`
- [x] **A resolver that fails the self-test disables checking and alerts — and pauses nothing**
- [x] UCEPROTECT L3 does not pause the node, with the measured reason recorded
- [x] A policy rate from one MX host does not pause the node; several distinct hosts alert
- [x] Policy replies still never lower a per-MX rate — the plan-003 test still passes
- [x] A paused node resumes through the operator path, with no redeploy
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated — pending manual sign-off

## Results (2026-08-28)

Gate clean.

**Against the deployed node's actual resolver** — the case the whole design is defensive about:

```
ERROR  blocklist checking disabled — resolver failed its self-test
       zen.spamhaus.org clean point unreachable:
       lookup 1.0.0.127.zen.spamhaus.org on 127.0.0.53:53: no such host
INFO   listening ...
```

The stub was refused, the service came up and served, and a probe afterwards returned `no_budget` —
**not** `ip_burned`. A resolver we cannot trust disables the check; it does not trigger one.

| Check | Result |
|---|---|
| No resolver configured | checking off, said plainly at startup, nothing paused |
| Host stub as the resolver | self-test refuses it, service serves, node not stood down |
| Resolver refusing every query (`127.255.255.254`) | refused at the test point |
| Resolver listing every query (`127.0.0.2`) | refused at the clean point |
| Confirmed listing, trusted resolver | node burned, reason names the zone, `ip:health:<ip>` written |
| Clean address | not burned |
| A DNSBL query that errors | not a listing |
| Five distinct MXes answering `5.7.x` | counted, node **not** burned |
| Twenty `5.7.x` from one host | counted as one host |
| `POST /admin/ip-health/resume` | clears the pause |
| `/admin/ip-health` without credentials | `401` |
| Default zones | no ASN-wide list among them |

## Notes / decisions / deviations

The RUNBOOK warns that DNSBL from the wrong resolver returns a false *clean*. In practice the more
dangerous direction is the opposite: a stub that answers `127.255.255.254` to everything reads as
*listed on every zone*, and a checker that pauses on that takes the node down for a resolver
misconfiguration. Hence the self-test, and hence the rule that a resolver we cannot trust disables
the check rather than triggering it.

The asymmetry between the two signals is deliberate. A listing is a fact about our IP that somebody
else published and that we can act on; a policy rate is an inference. Pausing on the inference would
mean any MX that starts answering `5.7.x` — misconfigured, hostile, or merely strict — could stand
our node down.
