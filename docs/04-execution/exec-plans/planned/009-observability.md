# Plan 009 — observability

**Status:** Planned
**Phase:** B
**Depends on:** 003

## Goal

Make the service legible in production: structured logs, Prometheus metrics, and request tracing, so
per-MX pacing, verdict mix, and IP health are visible on a dashboard and alertable.

## Context

The lab already exposes Prometheus text (`ds-smtp-retry/mxsim/internal/metrics`) with names the
validator shares. Reuse those names so existing dashboards work. Contract: `06-generated/metrics.md`.

## Design

- `internal/metrics` — Prometheus exposition at `GET /metrics` (operator-auth). Names aligned with
  the lab: `verify_requests_total{status}`, `verify_smtp_replies_total{code,class}`,
  `verify_rate_per_sec{mx_host}`, `verify_concurrency{mx_host}`, `verify_mx_state{mx_host,state}`,
  `verify_pause_events_total{mx_host}`, `verify_probe_blocked_total{reason}`,
  `verify_request_duration_seconds`, `ip_health_listed{ip,list}`.
- Structured JSON logs with a request id; SMTP transcripts at debug only; **never PII in info logs**
  (no full address at info — hash or domain-only).
- Lightweight tracing (request id propagated through resolve → pace → probe → classify).

## Tasks

- [ ] `internal/metrics` + `/metrics` endpoint (operator-auth)
- [ ] Instrument pacer, prober, resolver, iphealth
- [ ] Structured logger with request id; PII-safe levels
- [ ] Update `06-generated/metrics.md`, `operations/observability.md`, changelog

## Definition of Done

- [ ] A `/metrics` scrape shows per-MX rate, verdict counts, pause events
- [ ] Logs are JSON with request id; no full address at info level — checked
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Notes / decisions / deviations

Reuse the lab's metric names deliberately — one dashboard for lab + prod.
