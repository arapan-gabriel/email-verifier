# ADR-004 — Start with one probe; keep the bucket central

**Status:** Accepted (2026-08-26)

## Context

The user starts with a single sending node ("зонд") to isolate the main IP's reputation, but may add
nodes later. The rate limit belongs to the recipient MX, not to a node.

## Decision

- **Deploy one probe node at the start.** Multiple nodes are a later, deliberate scaling step, not a
  speed shortcut.
- **The token bucket is central in Redis from plan 003 on**, so scaling to N nodes changes only
  deployment, never the pacing model.

## Why

- Under safe pacing, more nodes do **not** raise the allowed rate to a given MX — the per-MX limit
  is the provider's, and one central bucket caps all nodes against it. `ds-smtp-retry`'s contract is
  explicit: *N probes with local buckets means N× the intended rate at Gmail.* Local buckets would
  silently multiply the rate and get the IPs blocked.
- What extra nodes actually buy is **reputation isolation** (one IP blocked, the rest continue) — a
  reason to add them, but a scaling/ops decision, not a throughput one.
- One machine is I/O-bound and paced; it saturates the achievable per-MX throughput on its own.

## Consequences

- Every plan is written so the pacing model is node-count-agnostic: take+refill in one round trip
  against the shared bucket (`token_bucket.lua`), never a per-process semaphore a second node cannot
  see.
- Adding node #2 is a deployment plan (new isolated IP + rDNS + SPF entry + its own `probeN`
  sub-domain), not an engine change. One domain, one sub-domain per node; SPF lists all node IPs.
- `iphealth` and pacing keys are keyed by MX (shared) and by node/IP (per node) so a burned node
  pauses itself without pausing the others.

## Alternatives rejected

- **Per-node local buckets for speed** — the exact anti-pattern the contract forbids; multiplies the
  real rate at the provider and burns IPs.
