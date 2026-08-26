# Metrics (generated)

Prometheus exposition at `GET /metrics` (plan 009). Names align with `ds-smtp-retry`'s where they
overlap, so the same dashboards work. Kept in sync with `internal/metrics` in the same change.

> Planned contract — populated as plan 009 lands.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `verify_requests_total` | counter | `status` | verdicts returned, by `valid/invalid/risky/unknown` |
| `verify_smtp_replies_total` | counter | `code`, `class` | raw SMTP replies by code and classification |
| `verify_rate_per_sec` | gauge | `mx_host` | current paced rate to each MX |
| `verify_concurrency` | gauge | `mx_host` | current concurrency to each MX |
| `verify_mx_state` | gauge | `mx_host`, `state` | 1 for the MX's current pacer state |
| `verify_pause_events_total` | counter | `mx_host` | per-MX pauses after repeated real throttles |
| `verify_probe_blocked_total` | counter | `reason` | probes skipped (redis_down, ssrf, suppressed) |
| `verify_request_duration_seconds` | histogram | — | end-to-end `/verify` latency |
| `ip_health_listed` | gauge | `ip`, `list` | 1 if the sending IP is on a blocklist |

## Alerts worth wiring (ops)

- `ip_health_listed == 1` → page: the IP is burned, sends should pause.
- sustained `verify_probe_blocked_total{reason="redis_down"}` → pacing is offline, all probes
  failing closed to `unknown`.
- `verify_pause_events_total` rate spike on one MX → that MX is throttling; band may be too high.
