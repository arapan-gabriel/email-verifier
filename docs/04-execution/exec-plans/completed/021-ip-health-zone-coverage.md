# Plan 021 — ip_health zone coverage: Abusix, and a self-test that fails one zone at a time

**Status:** Complete (2026-09-14)
**Phase:** C
**Depends on:** 020

## Goal

Let the node watch `combined.mail.abusix.zone` alongside Spamhaus and SpamCop, and make one
unreachable zone stop disabling the others. Today a third zone cannot be added safely, because
`SelfTest` is all-or-nothing.

## Context

**Why now.** Warm-up ladder day 4, 2026-09-12: `glowfish.de` answered
`550 5.7.1 Service unavailable; client [92.222.87.97] blocked using mail.abusix.zone`. Abusix's own
lookup confirms the listing — Guardian Mail, tagged `ip:new:smtp` and `spam:spam-source`. At that
moment `GET /admin/ip-health` reported `{"burned": false}` and `ip_health_listed` carried exactly
two series, both `0`.

Nothing malfunctioned. `ip_health.zones` is empty, which means `DefaultZones`, which is Spamhaus ZEN
plus SpamCop. But `burned: false` *means* "the two zones we check are clean" and *reads* as "this IP
is in good standing", and the first real reputation refusal of the ladder arrived through a
receiving server's reply text rather than through the check built to see it. That inverts the point
of the package: the node is supposed to know its own standing before it spends it.

**Why it is not a one-line config change.** `SelfTest` (`iphealth.go`) loops the zones and returns
on the first failure; `main.go` treats any error as "disable blocklist checking" and never starts
`health.Run`. So adding a third zone that cannot answer — an expired Abusix key, a rate-limited
reply, a zone rename — turns off the two that work. Adding coverage would strictly reduce safety,
which is the wrong trade for a package whose own doc-comment says a false positive here is a
self-inflicted outage.

**Invariants.** No change to 1-11. Config stays in `internal/config` (10). The Abusix key is a
credential and goes in the node's env file beside `VERIFIERD_SUPPRESS_SALT`, never in the tracked
YAML.

## Design

**Per-zone self-test.** `SelfTest` tests each zone independently, keeps the zones that pass, and
drops the ones that do not — returning the dropped zones and their reasons rather than an error, so
the caller can log what is and is not covered. It returns an error only when **no** zone passes,
which is the existing "the resolver cannot do DNSBL at all" case and keeps today's behaviour for
today's failure.

`Health.zones` is narrowed to the surviving set, so `Check` never queries a zone known to be
unanswerable and `ip_health_listed` carries a series only for zones actually checked. A zone absent
from the metric now means "not covered", which is the distinction the incident turned on.

**Abusix is configured, not defaulted.** `DefaultZones` stays Spamhaus + SpamCop: Abusix requires a
subscription key inside the query name (`<reversed-ip>.<APIKEY>.combined.mail.abusix.zone`), so a
keyless default would fail its self-test on every deployment that has no account. The existing
`prefix + "." + zone` construction already produces that name, so the zone string carries the key
and **no query code changes**.

`combined` rather than `black`: it aggregates the black, exploit and policy IP lists in one query,
which is what Abusix recommends for inbound decisions and what our `550` came from. Its documented
test point `127.0.0.2` is listed and `127.0.0.1` is not, so the existing self-test probes work
unchanged.

## Tasks

- [x] `SelfTest` returns `([]DroppedZone, error)`; narrows `h.zones` to the survivors
- [x] `main.go` logs covered and dropped zones separately; disables only when none survive
- [x] Unit tests: one zone stubbed broken keeps the others; all broken still disables; a stub
      resolver (answers everything) is still rejected per zone
- [x] `config/verifierd.yaml` comment documents the Abusix zone form and that the key is an env value
- [x] `docs/06-generated/metrics.md` — absence of a zone series now means "not covered"
- [x] `RUNBOOK.md` Phase 2 — check Abusix explicitly, the env line, where the key lives

## Definition of Done

- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean (0 issues)
- [x] `docs/05-quality/checklists/pr-checklist.md` confirmed — no SSRF surface, no verdict path, and
      the fail-closed rule is *strengthened*: an unanswerable zone is now dropped rather than
      silently taking the working zones down with it
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [x] **Manual gate:** with the key set on the node, `blocklist checking enabled` names three zones
      and `ip_health_listed` carries three series; with a deliberately wrong key, it names two and
      logs the third as dropped rather than disabling the check.
      **First half verified live 2026-09-14 10:32 UTC.** Guardian Mail Free account (5,000 queries
      *per week* — the node's 15-minute interval spends ~670), key tested through the node's unbound
      before it was written anywhere: the keyed test point answered `127.0.0.2` (among other
      `127.0.0.x`), `92.222.87.97` answered empty. `VERIFIERD_IP_HEALTH_ZONES` appended to
      `/etc/verifierd/env` (backup `env.bak-2026-09-14`), one restart: preflight GO WITH CAUTION
      (0 FAIL), `blocklist checking enabled` names all three zones, no `zone dropped`,
      `ip_health_listed` carries three series all `0`, `/admin/ip-health` = `{"burned":false}`.
      That first check ran on the 2026-09-12 04:45 UTC binary, older than `77e3e6b` and `224b0f9`,
      so it passed under the old all-or-nothing `SelfTest` and printed the key in full.
      **Deployed 2026-09-14 10:39 UTC** (run `34834239761`, `main` @ `224b0f9`, "deployed and
      healthy"): `blocklist checking enabled` names `<key>.combined.mail.abusix.zone`, the three
      `ip_health_listed` series are `0` with the key redacted in the label, `burned: false`, and the
      raw key appears zero times in the journal and the metrics since. Log lines from before the
      deploy still carry it.
      **Wrong-key half verified 2026-09-14 10:50 UTC**, on the deployed binary. The key in
      `/etc/verifierd/env` was replaced with 32 zeros and the node restarted (after waiting out the
      start-limit window the deploy's own restart had opened): `blocklist zone dropped — it failed
      its self-test` for `<key>.combined.mail.abusix.zone`, then `blocklist checking enabled` naming
      Spamhaus and SpamCop — the two survived instead of being disabled with it — two
      `ip_health_listed` series, `burned: false`. The good env was restored by an `EXIT` trap,
      byte-identical (`cmp`), and the restart after it named all three zones again.
      Two findings on the way, both in `tech-debt.md`: the dropped zone's `error` field carries the
      **unredacted** zone name, and the DNS error names `127.0.0.53` although the query went to
      unbound on `127.0.0.1`.
- [x] Status Complete, moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

**The Abusix account is a human step and blocks the manual gate, not the code.** The key comes from
a free Abusix registration — the same account that files the delisting request this incident also
needs. Until it exists the node runs two zones, exactly as before, and the code change is the part
that makes the third safe to add.

**Not doing: feeding a `550` back into health.** A receiving server naming our IP and a zone
(`client [<ip>] blocked using <zone>`) is the strongest possible signal and it currently dies in the
caller's `signals` column. Data Scout parses that text already (its plan 018). It is the right next
step and it is a different change: it crosses the repo boundary and it needs a rule for how much one
server's opinion may move our own state, which invariant 6 deliberately constrains.
