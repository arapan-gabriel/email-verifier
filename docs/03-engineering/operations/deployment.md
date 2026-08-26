# Operations — deployment

Detailed by plan 013. The isolated-IP host (Hetzner/OVH, open outbound `:25`).

- Prereqs are the RUNBOOK Phases 0–3: `:25` open, rDNS + FCrDNS, SPF/DKIM/DMARC, not blocklisted.
  `scripts/preflight.sh` must return GO before the service takes traffic.
- Distroless static binary; `docker-compose` with Redis; secrets via env/`internal/config`.
- Reachable from Data Scout over mTLS/API key only.
- Adding a second node = new IP + rDNS + SPF entry + `probeN` sub-domain; pacing unchanged (ADR-004).
