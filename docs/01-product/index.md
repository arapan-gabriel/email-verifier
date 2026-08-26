# Product — what email-verifier does

This service is infrastructure for the Data Scout platform, not a user-facing product of its own. Its
"features" are the capabilities it exposes to Data Scout over the HTTP boundary.

## Capabilities

| # | Feature | Phase | State |
|---|---|---|---|
| 001 | [Mailbox verification](features/001-verification.md) — `RCPT` probe, verdict `valid/invalid/risky/unknown` | A | planned |
| 002 | [Catch-all & randomiser detection](features/002-catch-all-detection.md) — `250` that means nothing → `risky` | A | planned |
| 003 | [Per-MX adaptive pacing](features/003-adaptive-pacing.md) — AIMD bands, central bucket, fail-closed | A | planned |
| 004 | [Bulk verification](features/004-bulk-verification.md) — job over a list, paced per MX | A | planned |
| 005 | [IP health & blocklist monitoring](features/005-ip-health.md) — detect a burned IP, pause | B | planned |
| 006 | [Outbound mail relay](features/006-mail-relay.md) — DKIM-signed transactional send | C | planned |

## What it deliberately does NOT do

- **Layers 0–5** (syntax, role, disposable, webmail, MX/DNS filtering) — these live in Data Scout,
  need no IP, and already exist. This service is called only for addresses that pass them.
- **Accept-all resolution** (Hunter/ZeroBounce's proprietary layer) — not reproducible; out of scope.
- **Storing verdicts or customer data** — Data Scout owns that. This service is stateless about
  business data.

## References

- [Verification build-vs-buy analysis](references/email-verification-build-vs-buy.md) — the Data
  Scout decision record that mandates a separate isolated-IP probe (the reason this repo exists).
