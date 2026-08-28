# ADR-003 — HTTP integration, stateless about business data

**Status:** Accepted (2026-08-26) · **transport shape and bulk handling superseded by
[ADR-006](ADR-006-batch-probe-seam.md) (2026-08-28)**; the "stateless about business data" decision
below still stands and ADR-006 carries it further.

## Context

Data Scout must call this service across a network boundary (it lives on a different host / IP). Two
questions: how they talk, and who owns the verdict data.

## Decision

- **Transport: HTTP now, shared queue later.** Data Scout's `email_verify.py` provider becomes a
  thin HTTP client to `POST /verify` (timeout + existing per-domain cache — Data Scout invariant
  10). Bulk gets a job endpoint, and a shared-queue worker mode is added later (ROADMAP 007) when
  volume warrants it.
- **This service is stateless about business data.** It returns a verdict and stores nothing about
  who asked or the answer. Data Scout owns the `email_verifications` table and the suppression list.
  This service owns only *operational* state (per-MX bands, IP health) in its own Redis.

## Why

- HTTP is the simplest correct boundary and matches Data Scout's existing provider pattern exactly —
  `email_verify.py` already wraps a provider with timeout and cache; swapping the implementation for
  an HTTP client is a small, well-understood change.
- Keeping verdict storage in Data Scout means one source of truth for a customer's data, one place
  for quota/GDPR/suppression, and a verifier that can die and be replaced without data loss — only
  the re-learnable working point is lost.
- Deferring the shared queue avoids coupling both systems to one broker/DB before the volume needs
  it; the engine is transport-agnostic (ENGINEERING-STANDARDS §2), so adding a queue worker later is
  additive.

## Consequences

- The HTTP contract (`docs/06-generated/api.md`) and the returned `source_ip` are part of the
  boundary: a verdict is only as good as the IP that produced it, so it always travels with it and
  Data Scout stores it.
- Auth is required on the edge (mTLS/API key).
- Bulk verification is a job (id + poll) until the queue mode lands.

## Alternatives rejected

- **Shared queue from day one** — more decoupled, but couples both systems to a shared broker/DB
  over the network before the volume justifies it, and complicates the first integration.
- **Verifier owns its own verdict DB** — gives the service autonomy but creates two sources of truth
  per address and duplicates GDPR/suppression responsibilities that already live in Data Scout.
