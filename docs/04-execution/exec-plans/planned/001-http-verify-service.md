# Plan 001 — http-verify-service

**Status:** Planned
**Phase:** A
**Depends on:** 000

## Goal

Port the SMTP prober from `ds-smtp-retry` and expose it as an authenticated `POST /verify` that
returns a verdict for a single address. This is the first real capability — the reason the isolated
IP exists.

## Context

`000` gave us the server skeleton, config, CI, healthz. The engine to port is
`../ds-smtp-retry/ratecheck/internal/prober` (one RCPT session + reply classification). Pacing is NOT
in this plan (it arrives in 003) — 001 uses a single serial probe with a fixed conservative rate so
the endpoint is testable end-to-end first. SSRF guard is 002; until then the resolver is trusted only
for `.test`/lab targets in tests.

## Design

- `internal/prober` — port from the lab: connect → `EHLO` → `MAIL FROM` → `RCPT TO` → `QUIT`,
  behind a `Prober` interface so tests never open a socket (mirror `smtp_probe.py`'s `set_prober`).
  **`DATA` is never sent.**
- Classifier ported verbatim (`Classify`, `classifyPermanent`, enhanced-code reader, `IsThrottle`/
  `IsTemp`) — it is the single reply→verdict source (`patterns/smtp-classification.md`).
- `internal/api` — `POST /verify {email, helo?, mail_from?}` → `{status, smtp_code, enhanced_code,
  catch_all, signals, source_ip, checked_at}`. Handler is thin: parse → auth → `prober.Verify` →
  serialise.
- **Auth (real):** mTLS or API-key middleware; every route but health. Replaces the 000 stub.
- Per-probe timeout from config; request context bound.
- **Dial `tcp4`, never `tcp`** (invariant 3) — an explicit config value (`dial_network`), not a
  default. The sending identity is published for IPv4 only; a dual-stack host would otherwise
  prefer IPv6 and connect from an address with no FCrDNS and no SPF. See Notes.
- `source_ip` resolved once at startup (or per config) and returned with every verdict.
- Verdict enforcement of invariant 1 lives in the classifier: only a `5.x` on `RCPT` after a good
  `MAIL FROM` yields `invalid`; everything else that fails → `unknown`.

## Tasks

- [ ] Port `internal/prober` (session + classifier) with the `Prober` interface + a fake for tests
- [ ] Port the enhanced-code classifier and `IsThrottle`/`IsTemp`
- [ ] `POST /verify` handler + request/response structs matching `06-generated/api.md`
- [ ] Real auth middleware (mTLS/API key); wire onto all non-health routes
- [ ] Config: probe timeout, HELO, MAIL FROM, source IP, `dial_network` (tcp4), auth secret
- [ ] Test asserting the dialer is address-family-pinned (no bare `tcp`)
- [ ] Table tests for the classifier; a `/verify` handler test with a fake prober
- [ ] Update `06-generated/api.md` (mark `/verify` live) and `changelog.md`

## Definition of Done

- [ ] `curl /verify` returns `valid` for a known-good and `invalid` for a known-bad address (verified
      against a lab MX / the `mxsim` simulator; record how)
- [ ] A rejection of us (timeout, 5xx before MAIL FROM) returns `unknown`, never `invalid` — test
- [ ] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] pr-checklist items confirmed (us ≠ address)
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated

## Notes / decisions / deviations

Fixed conservative rate here on purpose — real per-MX pacing is 003. Do not add ad-hoc semaphores
that a second node cannot see; leave pacing to the central bucket in 003.

**IPv4-only is not a preference, it is correctness** (measured on the deployed node, 2026-08-28).
The OVH VPS is dual-stack, and a plain `net.Dial("tcp", ...)` chose the IPv6 address on the first
try: Gmail was reached from `2001:41d0:404:200::169b`, whose PTR is the provider default and which
appears in no SPF record. Google accepts the connection (`220`) and rejects later with a `5.7.x`,
which this service classifies as `ClassPolicy` — so **every probe would return `unknown`** while
looking like a classifier bug rather than a network one. The same trap applies to any manual
testing: `swaks -4`, never bare `swaks`.

Either pin `tcp4`, or publish a full IPv6 identity (PTR + `AAAA` + `ip6:` in SPF). The second buys
nothing — many MX have no `AAAA` at all, and providers hold IPv6 senders to stricter rules.
