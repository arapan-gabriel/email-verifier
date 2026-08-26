# ADR-001 — Build on the ds-smtp-retry Go engine

**Status:** Accepted (2026-08-26)

## Context

Two verification engines already exist: Data Scout's in-process Python probe (`smtp_probe.py`, plan
065) with correct semantics but "4 ceilings" pacing, and the `ds-smtp-retry` Go lab with mature
per-MX AIMD calibration, rate bands, catch-all detection, RFC 3463 classification, and a central
Redis token-bucket contract — but packaged as a CLI. This service must run on an isolated IP as a
long-lived, network-IO-heavy edge that may scale to multiple probe nodes.

## Decision

Build `email-verifier` in **Go, on the `ds-smtp-retry` engine.**

## Why

- The hard parts — AIMD pacing, calibrated bands, the classifier, the central bucket contract — are
  already built and tested in the lab. Reimplementing them in Python would duplicate the most
  subtle code in the system.
- A single static binary on a Hetzner/OVH box with open `:25` is the ideal operational form for a
  network edge — trivial deploy, low footprint, thousands of concurrent SMTP sessions.
- Go's concurrency model fits per-MX pools and pacing better than an async Python worker for this
  workload.

## Consequences

- We must **reconcile verdict semantics** with Data Scout's existing statuses (plan 005) so the HTTP
  contract matches what `email_verify.py` and the `email_verifications` table already expect.
- The Python `smtp_probe.py` is retired once integration lands (plan 008); its semantics (us ≠
  address, SSRF guard, fail-closed) are carried over as invariants, not lost.
- New Go code is needed for what the lab lacks: SSRF guard, suppression, IP health, the HTTP service,
  auth, observability. See ROADMAP Phase A/B.

## Alternatives rejected

- **Deploy Python `smtp_probe` as a remote worker** — least new code, but carries the whole
  Python/Celery stack onto the edge and keeps the weaker "4 ceilings" pacing without calibration.
- **Hybrid (Go probe behind Python orchestration)** — maximum reuse but two languages to operate on
  the edge for no clear gain over a clean Go service.
