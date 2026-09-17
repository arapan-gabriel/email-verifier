# Operations — DNS

Detailed by plan 004. Resolver behaviour and caching: `docs/02-architecture/service/dns-resolver.md`.

- Own resolver config via `dns.servers` (empty = the host's). On the deployed node the host's is
  `systemd-resolved` on `127.0.0.53`, which is fine for A records and caches them. The RUNBOOK's
  warning about stubs bites on **DNSBL** lookups, which return a bogus "clean" through a stub or a
  public resolver — that is plan 010's problem, and it is why this knob exists.
- MX/A cache with TTL; no-MX and no-A handling; SSRF guard on results.

## Sending identity (the node's own DNS)

Distinct from resolution above: this is what the node publishes about *itself*. Concrete values and
the verified state: plan 013. The shape:

| Record | Name | Value | Note |
|---|---|---|---|
| PTR | the sending IP | `mail.<domain>` | set at the host provider, not in the zone |
| A | `mail.<domain>` | the sending IP | **never behind a CDN proxy** — it must resolve to the host |
| TXT | `<domain>` | `v=spf1 ip4:<ip> -all` | add `include:` before anything else sends from the domain |
| TXT | `probe.<domain>` | `v=spf1 ip4:<ip> -all` | the verification `MAIL FROM` domain |
| MX | `probe.<domain>` | the same routers as the root | **required**, see below |
| TXT | `_dmarc.<domain>` | `v=DMARC1; p=reject; sp=reject; rua=...` while the domain sends no message mail; `p=none; sp=none` only while you are still finding out | `sp=` is always set explicitly, so the subdomain policy is a decision rather than an inheritance |
| TXT | `<selector>._domainkey.<domain>` | `v=DKIM1; k=rsa; p=...` | Phase C only — verification signs nothing |

- **A domain that sends no message mail belongs at `p=reject`, and sooner than feels natural.**
  Verification sends `RCPT` without a body, so it has no `From:` and cannot appear in a DMARC report
  at all; the apex MX exist to *receive* bounces, which a sending policy does not touch. Everything
  a report does contain is therefore somebody else. `datascoutmail.com` sat at `p=none` until
  2026-09-17 and was forged in that window — NTT Docomo reported three messages from consumer
  addresses in India, Costa Rica and Argentina, SPF hardfail, unsigned, **delivered**, because the
  policy said to deliver them. It is now `p=reject; sp=reject`.
- **`sp=reject` covers `probe.` and every other subdomain, which is the point and also the trap.**
  Nothing sends from a subdomain today. When phase C's relay does, DKIM must be published *before*
  the first message: being listed in SPF is not alignment, and under `reject` an unsigned message
  whose envelope domain differs from its `From:` is refused rather than delivered to spam (plan
  `014`).
- **PTR and A must agree in both directions** (FCrDNS). Without it Yahoo, Apple, GMX and Microsoft
  reject before `RCPT` with a `5.7.x` that reads like a missing mailbox.
- **The identity is IPv4-only.** Verify with the address family pinned (`swaks -4`, prober `tcp4`) —
  otherwise a dual-stack host tests a path it will not use in production, or worse, uses a path it
  never published (ARCHITECTURE §Invariants).
- **The `MAIL FROM` sub-domain needs an MX of its own.** An SPF record alone is not enough: a server
  doing sender-domain verification looks up MX (then A) for the envelope sender's domain, finds
  nothing, and rejects with `554 5.1.8` — every probe, on every strict server. Measured against a
  real MX on 2026-08-28. Pointing it at the same routers as the root also makes sender *callouts*
  pass, not just the DNS existence check.
  **Deployed 2026-09-10** (plan 019): `probe.datascoutmail.com` MX 22/60/84 → `route3`/`route1`/
  `route2.mx.cloudflare.net`, and `mail_from` moved onto it. Only the existence half had ever been
  missing — the routers answered `250` to a `RCPT` for the sub-domain before any MX pointed at them.
  **Never substitute an A record**: with no MX, RFC 5321 falls back to A, which would name the probe
  node as its own implicit MX and send callouts at a closed inbound `:25`.
- Rotate DKIM by adding a new selector, never by replacing an existing key in place.
