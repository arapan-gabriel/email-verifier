# Plan 009 — observability

**Status:** Active
**Phase:** B
**Depends on:** 003

## Goal

Make the service legible in production: structured logs, Prometheus metrics, and request tracing, so
per-MX pacing, verdict mix, and IP health are visible on a dashboard and alertable.

## Context

The lab already exposes Prometheus text (`ds-smtp-retry/mxsim/internal/metrics`) with names the
validator shares. Reuse those names so existing dashboards work. Contract: `06-generated/metrics.md`.

## Design

### No Prometheus client library

`prometheus/client_golang` pulls protobuf, procfs and expfmt into a repository with exactly one
dependency. The exposition format is text, the metric set is fixed and small, and this repository
already hand-rolls its RESP client and its SMTP state machine for the same reason. `internal/metrics`
is a purpose-built registry — counters, gauges and one histogram — rendering the text format.

The one thing worth getting right by hand is the histogram: cumulative `le` buckets including
`+Inf`, plus `_sum` and `_count`. It has a test against a known distribution.

### Cardinality is a design constraint, not an afterthought

Four of the metrics in the contract are labelled by `mx_host`. A bulk run over ten thousand domains
would create a time series per MX host, forever — and it turns out **the pacer already has the same
problem**: `Pacer.mx` is a `map[string]*mxState` keyed by a value that arrives in the request and is
never evicted. Ten thousand domains means ten thousand entries held for the life of the process.

One fix serves both. The pacer's map becomes **bounded** — idle entries are evicted, and the map is
capped — because the working point lives in Redis and an evicted entry costs one re-read. The
metrics then expose exactly what the pacer is tracking, which is bounded by construction.

### What is actually measured

The contract was written before the code, and two entries no longer describe anything:
`verify_requests_total{status}` counts verdicts this service stopped producing under ADR-006 — it
returns facts and Data Scout scores them. It becomes `verify_results_total{class}`, over the classes
that exist.

| Metric | Type | Labels | Source |
|---|---|---|---|
| `verify_results_total` | counter | `class` | one per address answered |
| `verify_smtp_replies_total` | counter | `code`, `class` | one per reply read |
| `verify_probe_blocked_total` | counter | `reason` | `guarded`, `no_budget`, `paused`, `policy_stop` |
| `verify_pause_events_total` | counter | `mx_host` | the pacer standing an MX down |
| `verify_rate_per_sec` | gauge | `mx_host` | pulled from the pacer at scrape time |
| `verify_concurrency` | gauge | `mx_host` | pulled from the pacer at scrape time |
| `verify_mx_state` | gauge | `mx_host`, `state` | pulled from the pacer at scrape time |
| `verify_request_duration_seconds` | histogram | — | `POST /probe`, end to end |
| `verify_tracked_mx` | gauge | — | how many MXes the pacer holds; the cardinality canary |
| `go_goroutines` | gauge | — | a service that must not leak them should say how many it has |

Gauges are **pulled** from the pacer at scrape time rather than pushed: the pacer owns that state,
and mirroring it would give two answers that can disagree.

### Logging

Structured JSON with a request id, generated at the edge and carried through resolve → pace → probe
→ classify. **No address at info level** — the domain and a count, never the local part. Full SMTP
transcripts stay at debug.

## Deviation

`ip_health_listed{ip,list}` belongs to plan 010, which is where the state it reports comes from.
The registry has room for it; the metric arrives with the thing it measures.

## Tasks

- [x] `internal/metrics`: counters, gauges, a correct histogram, text exposition
- [x] **Bound the pacer's state map** — idle eviction and a cap; a test that it stops growing
- [x] `Pacer.Snapshot()` for the pull-at-scrape gauges
- [x] Instrument the prober: results, replies, blocked reasons, pauses
- [x] `GET /metrics` behind the same guard as any other non-health route (invariant 11)
- [x] Request id at the edge, carried through, present in every log line
- [x] No address at info level — a test asserting it
- [x] Update `06-generated/metrics.md`, `operations/observability.md`, changelog

## Definition of Done

- [ ] A `/metrics` scrape shows per-MX rate, verdict counts, pause events
- [ ] Logs are JSON with request id; no full address at info level — checked
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

## Results (2026-08-28)

Gate clean. A live scrape after three guarded requests and two fail-closed ones:

```
verify_results_total{class="guarded"} 3
verify_results_total{class="no_budget"} 4
verify_probe_blocked_total{reason="guarded"} 3
verify_probe_blocked_total{reason="no_budget"} 4
verify_rate_per_sec{mx_host="gmail-smtp-in.l.google.com"} 1
verify_mx_state{mx_host="gmail-smtp-in.l.google.com",state="PROBING"} 1
verify_tracked_mx 1
verify_request_duration_seconds_bucket{le="+Inf"} 5
verify_request_duration_seconds_count 5
go_goroutines 6
```

**`verify_tracked_mx 1` after three requests to `localhost`** is the earlier ordering fix visible in
a metric: a guarded target creates no pacer entry, so it produces no time series either.

| Check | Result |
|---|---|
| `/metrics` without credentials | `401` |
| `/metrics` with them | `200`, `text/plain; version=0.0.4` |
| Histogram against a known distribution | buckets cumulative, `+Inf` equals `_count`, `_sum` correct |
| 500 distinct MX hosts into a cap of 32 | 32 tracked; `Snapshot()` agrees with `Tracked()` |
| An evicted MX asked about again | resumes from the persisted rate, not the ceiling |
| Entry idle past the TTL | evicted, under `synctest` |
| Pause events | counted |
| Every request | one JSON line with `request_id`, echoed in `X-Request-Id` |
| Caller-supplied `X-Request-Id` | honoured, so a trace spans both services |
| An address in the logs | zero occurrences |

## Notes / decisions / deviations

Reuse the lab's metric names deliberately — one dashboard for lab + prod.

The unbounded pacer map was found while working out what to label the gauges with. It is a memory
leak on its own terms — a bulk run over ten thousand domains holds ten thousand entries for the life
of the process — and the reason it surfaced here is that a metric labelled by `mx_host` would have
made it visible as a cardinality problem instead of an invisible one. Fixing it is recorded as part
of this plan rather than deferred, because the metrics would otherwise ship a known footgun.
