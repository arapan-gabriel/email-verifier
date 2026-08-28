# Operations — deployment

Detailed by plan 013. The isolated-IP host (Hetzner/OVH, open outbound `:25`).

- Prereqs are the RUNBOOK Phases 0–3: `:25` open, rDNS + FCrDNS, SPF/DKIM/DMARC, not blocklisted.
  `scripts/preflight.sh` must return GO before the service takes traffic.
- One static binary (`CGO_ENABLED=0`) under `systemd`, unit from `packaging/verifierd.service`;
  Redis from the distro package on a unix socket (or loopback), never network-exposed. **No
  container runtime** (ADR-005). Secrets via env/`internal/config`.
- Debian 13, plain image, no control panel. No inbound MTA: nothing may listen on `:25`.
- Host choice matters more than hardware: a full region with outbound `:25` open, never an OVH
  Local Zone (SMTP is permanently unreachable there), and a `/24` that is not blocklisted.
- Reachable from Data Scout over mTLS/API key only.
- Adding a second node = new IP + rDNS + SPF entry + `probeN` sub-domain; pacing unchanged (ADR-004).
