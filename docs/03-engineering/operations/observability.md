# Operations — observability

Detailed by plan 009. Metrics contract: `docs/06-generated/metrics.md`.

- Prometheus at `/metrics`; names align with `ds-smtp-retry` so existing dashboards work.
- Structured logs (JSON) with request id; SMTP transcripts at debug level, never PII in info logs.
- Key alerts: IP listed, Redis-down fail-closed spike, per-MX pause spike.
