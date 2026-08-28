# Operations — DNS

Detailed by plan 004. Resolver behaviour and caching: `docs/02-architecture/service/dns-resolver.md`.

- Own resolver config (avoid a broken local stub — see the DNSBL note in the RUNBOOK).
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
| TXT | `_dmarc.<domain>` | `v=DMARC1; p=none; sp=none; rua=...` | `sp=` set explicitly so tightening `p=` later does not silently tighten `probe.` |
| TXT | `<selector>._domainkey.<domain>` | `v=DKIM1; k=rsa; p=...` | Phase C only — verification signs nothing |

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
- Rotate DKIM by adding a new selector, never by replacing an existing key in place.
