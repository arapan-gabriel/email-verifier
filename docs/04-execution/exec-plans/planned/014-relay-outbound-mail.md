# Plan 014 — relay-outbound-mail

**Status:** Planned
**Phase:** C
**Depends on:** 003, 010, 011, 013

## Goal

Send transactional mail from the isolated, warmed IP — DKIM-signed, paced per recipient MX, honouring
suppression and IP health — so the main product IP never sends outbound mail either.

## Context

Phase 2 of the scope (ADR-002). This reuses everything Phase A/B built: the per-MX pacer, IP health,
suppression. It adds real message assembly and signing. Data Scout (or its auth flow) hands off
transactional messages (welcome, reset — cf. Data Scout plan 049 SMTP provider). Component:
`service/mail-relay.md`.

## Design

- `POST /send {from, to, subject, headers, body}` (authenticated) → `{message_id, queued_at}`.
- `internal/relay` — assemble the message, **DKIM-sign** with the domain key, enqueue.
- Send queue drains through the **same per-MX pacer and central bucket** as verification (one pacing
  model for the IP), and checks **suppression (011)** and **IP health (010)** before each send.
- **Full SMTP delivery** (this DOES send `DATA`) — kept in a code path entirely separate from
  verification (invariant 8: verification never sends DATA).
- SPF/DKIM/DMARC alignment on the sending domain (identity from 013).
- Retry/backoff for transient send failures reuses the retry-queue pattern (006).

## Tasks

- [ ] `internal/relay`: message assembly + DKIM signing
- [ ] `POST /send` (auth) + send queue draining via the pacer/bucket
- [ ] Suppression + IP-health checks before each send
- [ ] Transient-failure retry (reuse 006 pattern)
- [ ] Tests: signed message assembly; suppressed/unhealthy → not sent
- [ ] Update `api.md`, `service/mail-relay.md`, `features/006`, `SECURITY.md`, changelog

## Definition of Done

- [ ] A signed test message is delivered and passes SPF + DKIM + DMARC (mail-tester) — recorded
- [ ] Verification and send are separate code paths; verify still never sends DATA — assert
- [ ] Suppressed/unhealthy send is blocked — test
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Sending reuses the verification IP's warmed reputation — sequencing verify first (Phase A) is what
makes this IP a credible sender. Keep verify/send strictly separated in code.
