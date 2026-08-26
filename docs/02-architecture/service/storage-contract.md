# Service — storage contract (with Data Scout)

This service holds **no business data** (ARCHITECTURE §"State ownership"). The verdict boundary:

- Response includes `source_ip` — a verdict is only as good as the IP that produced it; Data Scout
  stores it in `email_verifications.signals`.
- `status ∈ {valid, invalid, risky, unknown}`; the reconciliation to Data Scout's status vocabulary
  is defined in plan 005.
- Data Scout's `app/core/providers/email_verify.py` wraps calls with a timeout and its existing
  per-domain cache (Data Scout invariant 10). Integration lands in plan 008.
- Suppression source of truth is Data Scout; this service reads a synced copy/endpoint (plan 011).
