# Operations — DNS

Detailed by plan 004. Resolver behaviour and caching: `docs/02-architecture/service/dns-resolver.md`.

- Own resolver config (avoid a broken local stub — see the DNSBL note in the RUNBOOK).
- MX/A cache with TTL; no-MX and no-A handling; SSRF guard on results.
