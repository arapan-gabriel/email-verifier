# AGENTS — orientation

Start here if you are an agent or a new contributor.

## What this repo is

`email-verifier` — a Go service on an isolated IP that does SMTP mailbox verification and (phase 2)
outbound mail relay for the Data Scout platform. It exists to keep port-25 reputation off the main
product host. Full picture: `../README.md`, `docs/02-architecture/ARCHITECTURE.md`.

## The governing file

**`CLAUDE.md`** (repo root) is binding: the five-phase work cycle and the hard invariants. Read it
before doing anything. This file is the short orientation; `CLAUDE.md` is the law.

## Invariants you cannot break (summary — full text in CLAUDE.md)

1. A rejection of *us* is never a verdict about the address → `unknown`, never `invalid`.
2. Never connect to a private/loopback MX (SSRF guard).
3. Dial `tcp4`, never `tcp` — the sending identity is published for IPv4 alone.
4. The rate bucket is central (Redis); pacing is shared across nodes.
5. Rate ceilings fail closed — no Redis, no probe.
6. A `5.7.x` policy block about our IP is not throttling and not `invalid`.
7. `250` on a catch-all/randomiser is `risky`, not `valid`.
8. Never send `DATA` during verification.
9. Honour the suppression list.
10. Config from one place (`internal/config`).
11. The HTTP edge is authenticated except `/healthz` and `/readyz`.

These numbers are the ones every other doc cites. They are defined in `CLAUDE.md`; this list only
mirrors them, so the two must be renumbered together.

## Agent rules

**No hand-run SMTP from the node.** Never open an SMTP connection to a real MX from
the verifier node (`92.222.87.97`) yourself: no `openssl s_client -starttls smtp`, `swaks`,
`telnet`, `nc`, `curl smtp://` or ad-hoc script, over ssh or otherwise, even "just to read the TLS
offer". On 2026-10-02 an agent's diagnostic did exactly that, under openssl's default HELO, and got
the IP listed on Spamhaus CSS within the hour. The host firewall now refuses SMTP from any user but
`verifierd`; do not work around it. Diagnose from `mxsim` or a host that does not probe, or ask the
owner to run a `POST /probe` through the service — `docs/07-references/RUNBOOK.md`.

## Where to look

| You want to… | Go to |
|---|---|
| Know what's next to build | `docs/04-execution/ROADMAP.md` |
| Understand the system | `docs/02-architecture/ARCHITECTURE.md` |
| Follow the golden path | `docs/02-architecture/ENGINEERING-STANDARDS.md` |
| Know why a choice was made | `docs/02-architecture/decisions/ADR-*.md` |
| Run against a real IP safely | `docs/07-references/RUNBOOK.md` |
| Understand a reply → verdict | `docs/03-engineering/patterns/smtp-classification.md` |
| Understand pacing | `docs/03-engineering/patterns/aimd-pacing.md` |

## Related repos

- `../ds-smtp-retry` — the calibration lab this service's engine is ported from (prober, pacer,
  limiter contract, classifier, preflight, probe/ladder lists).
- `../data-scout` — the platform that calls this service; owns layers 0–5, quota, verdict storage,
  suppression. Its `apps/api/app/core/providers/email_verify.py` becomes the HTTP client (plan 008).
