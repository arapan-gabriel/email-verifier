# ADR-002 — Scope: verification probe + outbound relay (phased)

**Status:** Accepted (2026-08-26)

## Context

The isolated IP exists to keep port-25 reputation off the main product host. Two workloads need that
isolation: SMTP verification (`RCPT` probing) and outbound transactional mail (welcome, reset,
notifications) — both spend the IP's sending reputation.

## Decision

This service owns **both**, delivered in phases: **verification first** (ROADMAP Phase A/B),
**outbound relay second** (Phase C). It does **not** own the cheap local checks (layers 0–5) — those
stay in Data Scout, which calls this service only for addresses that survive them.

## Why

- Isolating verification but leaving transactional mail on the main IP would only half-solve the
  reputation problem the whole project is about.
- Verification and relay share the same needs — open `:25`, rDNS/FCrDNS/SPF/DKIM, per-MX pacing, IP
  health — so one service, one IP identity, one pacing model.
- Verification is sequenced first because it is the immediate need and because a clean, paced
  verification track is the best way to warm the IP before it sends real mail.

## Consequences

- The pacing/limiter/IP-health components are built once and reused by relay (Phase C reuses Phase
  A/B infrastructure).
- Relay adds DKIM signing, a send queue, and bounce handling (plans 014–015) — genuinely new
  surface, kept behind an authenticated `POST /send` separate from verification.
- Layers 0–5 are explicitly **out of scope** (they live in Data Scout, need no IP, and already
  exist), avoiding duplication of syntax/disposable/MX/webmail logic.

## Alternatives rejected

- **Probe-only** — leaves transactional mail on the main IP; does not meet the stated goal ("и слать
  письма" from the isolated IP).
- **Full pipeline 0–8 here** — duplicates the local layers Data Scout already runs for free, with no
  IP benefit.
