# Plan 013 — deployment

**Status:** Planned
**Phase:** B
**Depends on:** 001, 003, 009

## Goal

Deploy the service on the real isolated-IP host with a correct sender identity, reachable from Data
Scout over an authenticated link, gated on the preflight passing GO.

## Context

The host is a Hetzner/OVH box with open outbound `:25` (build-vs-buy §3.1 names these as the ones
that actually allow it). Sender identity setup is the RUNBOOK Phases 0–3; `scripts/preflight.sh`
(ported in 000) is the go/no-go gate. Component: `operations/deployment.md`.

## Design

- Provision the isolated IP: open `:25`, rDNS `mail.<domain>` + FCrDNS, SPF (`ip4:<ip>`), DKIM,
  DMARC — the full RUNBOOK Phase 1. HELO/`mail_from` config point at these names.
- Distroless static image; `docker-compose` (verifierd + Redis) or systemd unit; secrets via
  env/`internal/config`.
- **mTLS/API key** link from Data Scout to this host (private network or authenticated public
  ingress). No unauthenticated surface except health.
- **Deploy gate:** `scripts/preflight.sh <domain> mail.<domain> <dkim-selector>` must return GO on
  the real host before the service takes traffic. Wire it as a pre-start check / CI deploy gate.
- One-probe topology (ADR-004); document the "add node #2" steps (new IP + rDNS + SPF entry +
  `probeN` sub-domain) without implementing them.

## Tasks

- [ ] Production image + compose/systemd; secrets via config
- [ ] Host sender identity: `:25`, rDNS/FCrDNS, SPF/DKIM/DMARC (RUNBOOK Phase 1)
- [ ] mTLS/API-key link to Data Scout
- [ ] Preflight as a pre-start/deploy gate
- [ ] Update `operations/deployment.md`, `SECURITY.md`, changelog

## Definition of Done

- [ ] `scripts/preflight.sh` returns GO on the real host (recorded)
- [ ] Service reachable from Data Scout over mTLS/API key; health green
- [ ] A single `/verify` against a real MX returns a correct verdict from the isolated IP
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Do not skip the preflight gate — a deploy onto a blocked/reverse-DNS-broken IP produces verdicts that
measure nothing (RUNBOOK). GO is a hard precondition, not advisory.
