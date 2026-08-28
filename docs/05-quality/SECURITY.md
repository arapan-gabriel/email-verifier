# Security

This service is safe-by-design to point at strangers' MXes; a bug that floods someone's MX is the
highest-severity class here.

- **SSRF guard** on every resolved MX IP (invariant 2, `patterns/ssrf-guard.md`).
- **IPv4 only** (invariant 3) — the published sending identity covers the IPv4 address; a bare
  `tcp` dial on a dual-stack host silently sends from an unidentified one.
- **Fail closed** on the pacing path (invariant 5) — no Redis, no send.
- **Authenticated edge** — mTLS/API key on every non-health endpoint.
- **No DATA during verification**; relay is a separate authenticated path.
- **Suppression** honoured before any probe/send (GDPR, invariant 9).
- No business data at rest; secrets via config only, never logged.
