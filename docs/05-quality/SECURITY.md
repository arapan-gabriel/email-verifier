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

## As deployed (plan 013, `92.222.87.97`)

- **The edge is mTLS *and* an API key.** A private CA issued on the host; `tls.client_ca_file` is
  set, so the handshake requires a client certificate. Verified: no certificate →
  `tlsv13 alert certificate required`; a certificate from another CA → `tlsv1 alert unknown ca`.
  Both are TLS alerts — the request never reaches a handler. A valid certificate without the API
  key is a `401`.
- **The API port is closed at the firewall.** `ufw` permits `22/tcp` only; `0.0.0.0:8443` is
  reachable from the host alone. Who may reach it is plan 008's decision (the caller's address is
  probably dynamic, and a rule pinned to it would fail silently when it rotates).
- **The CA private key is on the host it protects** (`/etc/verifierd/tls/ca.key`, root-only). That
  is the weak point of this arrangement and it is deliberate for now: moving it to offline storage
  costs nothing and should happen before the link carries production traffic.
- The only secret in the environment is `VERIFIERD_AUTH_API_KEY`, in a `640 root:verifierd`
  `EnvironmentFile` — never in the unit, never in the repo (invariant 10).
- **Two protections are switched off on the node, by configuration, and that is visible in the
  startup log rather than hidden:**
  - *Blocklist self-monitoring* (invariant-adjacent, plan 010) is off because no DNSBL-capable
    resolver is configured. The host's stub cannot answer DNSBL queries and the major zones refuse
    public resolvers, so turning it on needs a local recursive resolver. Until then the node will
    not notice its own listing.
  - *The local suppression copy* (invariant 9) is off, so Data Scout's list is the only one. That
    matches the design — this was always the second line, and the authoritative check runs three
    times upstream — but the second line is not currently there.
- No inbound MTA and nothing listening on `:25`; Redis is on a unix socket with `port 0`, so it is
  not on the network at all.
