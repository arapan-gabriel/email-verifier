# Operations — IP reputation & health

Detailed by plan 010. The IP is the asset; protecting it is continuous ops.

- `internal/iphealth` self-monitors blocklists (Spamhaus etc.); `ip:health:<ip>` in Redis.
- A listing flips health and pauses sends/probes for that node (fail-safe).
- rDNS/FCrDNS must stay valid; PTR change is an incident.
- Verification traffic looks like harvesting to providers — pacing (Phase A) is what keeps the IP
  clean. See `docs/07-references/RUNBOOK.md` for delisting and setup.
