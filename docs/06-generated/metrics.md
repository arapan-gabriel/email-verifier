# Metrics (generated)

Prometheus exposition at `GET /metrics` (plan 009). Names align with `ds-smtp-retry`'s where they
overlap, so the same dashboards work. Kept in sync with `internal/metrics` in the same change.

**Live** (plan 009). Exposed at `GET /metrics`, behind the same guard as any other non-health
route (invariant 11).

**No Prometheus client library.** It pulls protobuf, procfs and expfmt into a repository with one
dependency; the exposition format is text and the metric set is fixed, so `internal/metrics` renders
it directly — as this repository already does for RESP and for SMTP.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `verify_results_total` | counter | `class` | one per address answered, by classification |
| `verify_smtp_replies_total` | counter | `code`, `class` | one per reply actually read |
| `verify_probe_blocked_total` | counter | `reason` | probes declined: `guarded`, `no_budget`, `paused`, `policy_stop` |
| `verify_pause_events_total` | counter | `mx_host` | the pacer standing an MX down at the floor of its band |
| `verify_rate_per_sec` | gauge | `mx_host` | rate the AIMD loop has settled on |
| `verify_concurrency` | gauge | `mx_host` | concurrency it has settled on |
| `verify_mx_state` | gauge | `mx_host`, `state` | 1 for the MX's current state |
| `verify_request_duration_seconds` | histogram | — | end-to-end `POST /probe` |
| `verify_tracked_mx` | gauge | — | MX hosts the pacer holds state for — **the cardinality canary** |
| `go_goroutines` | gauge | — | a service that must not leak them should say how many it has |

| `ip_health_listed` | gauge | `ip`, `list` | 1 if this sending address is on the named blocklist |

`ip_health_listed` appears only once a check has run. Absent means checking is off — which is the
default, and deliberately so: without a resolver that can answer DNSBL queries, checking stays
disabled rather than trusting the host's stub.

## Cardinality

Four metrics are labelled by `mx_host`, which arrives in the request. Every one of them is bounded
by **`verify_tracked_mx`**: the pacer evicts idle MXes and caps how many it holds, so the label set
cannot grow without limit. If that gauge climbs toward `pacer.max_tracked` and stays there, eviction
has stopped working and the series count is about to follow — alert on it.

Gauges are **pulled from the pacer at scrape time**, not mirrored: it owns that state, and a copy
would give two answers that can disagree.

## Alerts worth wiring (ops)

- sustained `verify_probe_blocked_total{reason="no_budget"}` → **page**: the bucket is unreachable,
  every probe is failing closed and answering nothing. `/readyz` is already 503.
- `verify_probe_blocked_total{reason="guarded"}` rising → someone is pointing MX records inward, or
  a caller is sending internal hosts. Not an outage, but worth knowing.
- `verify_probe_blocked_total{reason="policy_stop"}` rising → servers are refusing our client. If it
  spreads across MXes it is the IP, not the servers — that is plan 010's signal.
- `verify_pause_events_total` rate spike on one MX → that MX is throttling; its band may be too high.
- `verify_tracked_mx` pinned at `pacer.max_tracked` → eviction has stopped working; per-MX series are
  about to grow without limit.
- `ip_health_listed == 1` (plan 010) → page: the IP is burned, sends should pause.
