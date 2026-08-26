# Plan 015 — relay-bounce-handling

**Status:** Planned
**Phase:** C
**Depends on:** 010, 011, 014

## Goal

Close the loop on sending: capture bounces and complaints, feed them into IP health, and suppress
hard-bounced addresses — so the isolated IP stays clean and a dead address is never mailed twice.

## Context

A sender that ignores bounces gets its IP burned. This plan handles the return path of the relay
(014): hard bounces, soft bounces, and complaints (feedback loops). Feeds `iphealth` (010) and
`suppress` (011). Component: `service/mail-relay.md`.

## Design

- Ingest bounces: a return-path/VERP mailbox or webhook the relay controls; parse DSN
  (`5.x.x` hard, `4.x.x` soft) and feedback-loop complaints.
- **Hard bounce (`5.1.x`)** → mark the address, push a suppression signal back to Data Scout (source
  of truth) and to the local `suppress` set; never mail it again.
- **Soft bounce** → bounded retry via the 006 queue, then give up.
- **Complaint (FBL)** → suppress + weight IP health down; a rising complaint rate pauses sending
  (010).
- Bounce metrics into 009 (`relay_bounces_total{class}`, complaint rate) for alerting.

## Tasks

- [ ] Bounce ingest (VERP mailbox or webhook) + DSN parsing
- [ ] Hard bounce → suppress (local + push to Data Scout)
- [ ] Soft bounce → bounded retry; complaints → suppress + IP-health penalty
- [ ] Metrics + alerts for bounce/complaint rate
- [ ] Tests: hard bounce suppresses; complaint penalises IP health
- [ ] Update `service/mail-relay.md`, `operations/ip-reputation.md`, `metrics.md`, changelog

## Definition of Done

- [ ] A hard bounce marks the address and updates suppression (local + Data Scout) — test
- [ ] A complaint penalises IP health; a complaint spike pauses sending — test
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Suppression source of truth stays Data Scout (011) — this plan pushes signals to it, and also keeps a
local copy for immediate enforcement. This is the last plan of Phase C; the IP is now a self-policing
sender.
