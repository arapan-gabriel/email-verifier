# Plan 002 — ssrf-guard-and-safety

**Status:** Planned
**Phase:** A
**Depends on:** 001

## Goal

Make it impossible to open a socket to a private/loopback/link-local address from a
domain-controlled MX record, and lock down the "us ≠ address" verdict rules with regression tests.

## Context

The MX host is attacker-influenced (a domain owner can point MX at `127.0.0.1` or cloud metadata).
The `ds-smtp-retry` lab deliberately connects to loopback — that is the ONE behaviour we invert here.
Mirrors Data Scout's plan-024 guard. Pattern: `patterns/ssrf-guard.md`.

## Design

- `internal/resolver` gains an SSRF guard applied to every resolved IP before it reaches the prober.
  Reject: loopback, private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`), link-local
  (`169.254/16` incl. cloud metadata, `fe80::/10`), unspecified, multicast, reserved.
- A domain whose MX resolves only to blocked IPs → `unknown` with a reason (our refusal, not the
  mailbox's absence) — never `invalid`.
- The prober can only receive vetted IPs — the guard is not a caller responsibility.
- Add regression tests for the full "us ≠ address" matrix: conn refused, TLS error, timeout, 4xx,
  5xx-before-MAIL-FROM all → `unknown`; only 5xx-on-RCPT-after-good-MAIL-FROM → `invalid`.

## Tasks

- [ ] IP-range guard util + unit tests (v4 + v6 ranges)
- [ ] Wire the guard into resolution so the prober only gets routable IPs
- [ ] `unknown`+reason path when all MX IPs are blocked
- [ ] Regression suite for the us≠address matrix (stub prober/transport)
- [ ] Update `service/dns-resolver.md`, `changelog.md`

## Definition of Done

- [ ] A probe of a domain whose MX points at `127.0.0.1` (stub resolver) is refused, not attempted —
      test
- [ ] Cloud-metadata IP (`169.254.169.254`) blocked — test
- [ ] us≠address matrix all green
- [ ] gates clean; pr-checklist SSRF + us≠address items confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

This is the single most important safety plan — a bug here is either an SSRF hole or a
list-destroyer. Prefer over-blocking (a rare false `unknown`) to ever connecting inward.
