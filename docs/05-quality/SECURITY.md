# Security

This service is safe-by-design to point at strangers' MXes; a bug that floods someone's MX is the
highest-severity class here.

- **SSRF guard** on every resolved MX IP (invariant 2, `patterns/ssrf-guard.md`).
- **IPv4 only** (invariant 3) — the published sending identity covers the IPv4 address; a bare
  `tcp` dial on a dual-stack host silently sends from an unidentified one.
- **Fail closed** on the pacing path (invariant 5) — no Redis, no send.
- **Authenticated edge** — mTLS/API key on every non-health endpoint.
- **No DATA during verification**; relay is a separate authenticated path.
- **Suppression** honoured before any probe or send (GDPR, invariant 9) — and **as digests, never
  as addresses**. Data Scout owns the list and checks it three times before calling; this is a
  second line. Copying the addresses here would put personal data at rest on a second host, for a
  mechanism whose purpose is erasure. What is stored is `sha256(salt + "\x00" + value)`: membership
  is checkable, the plaintext is not recoverable, and erasure is deleting one key. Still
  pseudonymised rather than anonymous — the point is a smaller blast radius, not a discharged
  obligation.
- A stale or unreadable local copy is **loud, not fatal**, on the verify path: the authoritative
  check has already run. Phase C relay fails closed instead, because sending is irreversible and has
  no upstream check between the queue and the socket.
- No business data at rest; secrets via config only, never logged.
