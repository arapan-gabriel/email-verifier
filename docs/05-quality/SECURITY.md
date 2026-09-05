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
- **The API port is closed at the firewall — `nftables`, since 2026-09-05.** `/etc/nftables.conf`,
  table `inet filter`, input `policy drop`: established/related, loopback, ICMP, `22/tcp` from
  anywhere, and `8443/tcp` from Data Scout's caller alone. Output stays `accept` — this host exists
  to open outbound SMTP sessions, and the established rule is what keeps their replies flowing.
  `nftables.service` is enabled, so the ruleset survives a reboot. Verified after applying:
  outbound `:25` still reaches Gmail, a fresh SSH connection still lands, and the policy counter is
  already collecting scanner traffic.

  **This claim used to be false, and how it was false is the point.** It read "`ufw` permits
  `22/tcp` only". `ufw status` did report `active` — but it reads that from `ENABLED=yes` in its own
  config file, and **neither `nft` nor `iptables` was installed**, so `ufw` could not write one rule
  into the kernel; `ufw.service` was `inactive (dead)`. The host had no packet filter at all. What
  actually dropped the traffic was OVH's edge, off the box and outside this repository. Measured
  from outside: `22` open, while `80`, `8443` **and `8444`, where nothing listens**, all timed out
  instead of being refused — which is only possible if the packets never reach the host. A firewall
  that reports intent rather than enforcement is worse than none, because it gets quoted in
  documents like this one. `ufw` is now disabled outright so two systems cannot fight at boot.

  Who may reach `8443` is still plan 008's decision. The rule names the caller's public address
  today; that address is on a consumer line and may rotate, which is a live risk rather than a
  settled question — see 008.
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
- **What a deploy credential can and cannot reach** (plan 016). `deploy` is key-only, has no
  password, and `sudo -l` shows exactly one permitted command: a root-owned, argument-less script.
  It **cannot** read `/etc/verifierd/tls/ca.key`, **cannot** read or write `/etc/verifierd/env`, and
  has no general `sudo` — all four verified, not assumed. What it *can* do, unavoidably, is replace
  the binary that runs as `verifierd`, which reads `server.key` and `ca.pem`. The CA private key is
  root-only and stays outside that radius.
- **The privileged script never ships in the release bundle.** The staging directory is writable by
  `deploy`, so a bundle carrying `verifierd-deploy` — and a deploy that installed it — would let
  `deploy` place arbitrary code where root runs it, defeating the one-line `sudoers` entry entirely.
  Updating that script is a manual, root action.
- **The start gate is rate-limited** (`StartLimitBurst=3`, `StartLimitIntervalSec=600`). `ExecStartPre`
  performs a live SMTP handshake, so an unbounded `Restart=on-failure` would dial a real provider
  every few seconds forever in exactly the case the gate exists to catch — a host whose identity is
  already broken. That is how a stuck unit turns a DNS mistake into a reputation problem.
- No inbound MTA and nothing listening on `:25`; Redis is on a unix socket with `port 0`, so it is
  not on the network at all.
