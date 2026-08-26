# Service — resolver (`internal/resolver`)

MX/A resolution with cache and the SSRF guard. Detailed by plan 004.

- No-MX fallback (implicit MX = A record); no A and no MX → "no mail server" (not `invalid`).
- SSRF guard on every resolved IP: `docs/03-engineering/patterns/ssrf-guard.md` (invariant 2).
- Cache in Redis (`dns:mx:<domain>`), TTL-bound.
- The prober only ever receives vetted, routable IPs — the guard cannot be bypassed by a caller.
