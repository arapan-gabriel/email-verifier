# Operations — observability

**Live** (plan 009). Metrics contract and alerts: `docs/06-generated/metrics.md`.

- Prometheus text at `GET /metrics`, behind the same guard as any non-health route. Hand-rolled —
  no client library, for the same reason the RESP client and the SMTP state machine are.
- Names align with `ds-smtp-retry` where they overlap, so one dashboard serves lab and production.
- **Per-MX labels are bounded by construction.** The pacer evicts idle MXes and caps how many it
  holds, and `verify_tracked_mx` reports the ceiling being approached. Without that, a bulk run over
  ten thousand domains would leave a time series per host, forever.
- Structured JSON logs, one line per request, each carrying a `request_id`. A caller-supplied
  `X-Request-Id` is honoured and echoed back, so a trace spans Data Scout and this service.
- **No address at info level.** The domain and a count are enough to find a problem; the local part
  is the customer's data. Full SMTP transcripts stay at debug. A test asserts it.
