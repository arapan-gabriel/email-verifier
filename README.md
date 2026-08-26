# email-verifier

Isolated-IP mail edge for the Data Scout platform. One Go service that owns everything touching
outbound **port 25**, so the main product IP never spends its reputation on it:

- **Verify** — SMTP `RCPT TO` mailbox probing with per-MX AIMD pacing, catch-all detection, and
  RFC 3463 reply classification. Never sends `DATA`.
- **Relay** (phase 2) — outbound transactional mail from the same isolated, warmed IP, DKIM-signed.

Built on the calibration engine from [`ds-smtp-retry`](../ds-smtp-retry) (AIMD bands, the central
Redis token-bucket contract, the reply classifier) and the production semantics of Data Scout's
in-process probe (`smtp_probe.py`, plan 065), which this service replaces.

## Why a separate service

Data Scout's own build-vs-buy analysis is explicit: the SMTP probe **must** run on separate machines
with an open `:25`, correct rDNS/FCrDNS, and its own IP reputation — outside the production host
behind Cloudflare Tunnel. This repo is that machine.

```
Data Scout API  ──HTTP (mTLS/API-key)──►  email-verifier (this repo)
 layers 0–5, quota,                        isolated IP · :25 open · rDNS/FCrDNS/SPF/DKIM
 email_verifications table                 verify + relay · per-MX bands + IP health in Redis
```

Data Scout keeps the cheap local checks, quotas, and verdict storage. This service is **stateless
about business data** and owns only operational state.

## Status

Greenfield. Architecture is locked (see `docs/02-architecture/ARCHITECTURE.md` and the ADRs);
execution is sequenced in `docs/04-execution/ROADMAP.md`. Start at plan `000`.

## Docs

Mirrors the Data Scout documentation layout. Entry points:

- **`CLAUDE.md`** — how to work in this repo; the five-phase cycle and hard invariants.
- **`docs/02-architecture/ARCHITECTURE.md`** — the system, the codemap, "where is X".
- **`docs/04-execution/ROADMAP.md`** — the plan sequence.
- **`docs/07-references/RUNBOOK.md`** — cold-IP-to-first-run operational runbook.
- **`docs/00-meta/DOCS_GUIDE.md`** — what each `docs/` folder is for.
