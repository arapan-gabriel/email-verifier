# Service — storage contract (with Data Scout)

This service holds **no business data** (ARCHITECTURE §"State ownership"). The verdict boundary:

- Response includes `source_ip` — a verdict is only as good as the IP that produced it; Data Scout
  stores it in `email_verifications.signals`.
- `status ∈ {valid, invalid, risky, unknown}`; the reconciliation to Data Scout's status vocabulary
  is defined in plan 005.
- Data Scout's `app/core/providers/email_verify.py` wraps calls with a timeout and its existing
  per-domain cache (Data Scout invariant 10). Integration lands in plan 008.
- Suppression source of truth is Data Scout, which checks it three times before calling. This
  service keeps a **digest-only** second line, pushed to `POST /admin/suppress` (plan 011). It never
  holds an address: what is stored is a salted SHA-256, so membership is checkable and the plaintext
  is not recoverable from this host.
