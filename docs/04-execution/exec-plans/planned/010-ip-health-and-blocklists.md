# Plan 010 — ip-health-and-blocklists

**Status:** Planned
**Phase:** B
**Depends on:** 003, 009

## Goal

Notice when the sending IP is burned — blocklisted or being policy-refused — and pause its traffic
automatically, before it does more damage. The IP is the asset; this protects it.

## Context

A `RCPT`-heavy IP gets listed in hours (build-vs-buy §3.4). The lab already treats `ClassPolicy`
(`5.7.x` / `554 blocked`) as an IP signal, not a rate one. This plan turns that into a health state
and an operational response. Component: `operations/ip-reputation.md`.

## Design

- `internal/iphealth` — track per-IP signals: a rising rate of `ClassPolicy` / `blocked` replies,
  and periodic DNSBL checks (Spamhaus etc.) from a resolver that can query them (RUNBOOK notes
  public resolvers are refused). State in Redis `ip:health:<ip>`.
- **On listed/burned:** flip health, pause sends/probes for that node (fail-safe, like the per-MX
  pause but IP-wide), emit `ip_health_listed=1`, and alert (page).
- A policy block never drives per-MX AIMD (invariant 5) — it feeds IP health instead. Keep that
  separation.
- Multi-node ready (ADR-004): health is per-IP, so one burned node pauses itself without pausing the
  others.

## Tasks

- [ ] `internal/iphealth` — policy-reply rate + DNSBL check; `ip:health:<ip>` in Redis
- [ ] Auto-pause node traffic on burned; resume path when clear
- [ ] `ip_health_listed` metric + alert wiring (from 009)
- [ ] Tests: simulated listing flips health and pauses; policy reply does not touch AIMD
- [ ] Update `redis-contract.md`, `operations/ip-reputation.md`, changelog

## Definition of Done

- [ ] A simulated blocklist listing flips IP health and pauses sends — test
- [ ] Policy replies feed health but never lower a per-MX rate — test
- [ ] gates clean; pr-checklist (policy ≠ throttle) confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

DNSBL from the wrong resolver returns false "clean" (RUNBOOK) — use a resolver allowed to query, and
treat provider `550 blocked` text as authoritative alongside DNSBL.
