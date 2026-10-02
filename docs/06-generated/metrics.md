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
| `verify_smtp_replies_total` | counter | `code`, `class` | one per reply actually read. **No `mx_host` label, deliberately** — it is the obvious next thought and it is wrong: the label would be unbounded by request input, which is the cardinality this file's per-MX gauges are carefully bounded against. The per-host record is the `smtp_reply` log line (plan 018) |
| `relay_complaints_total` | counter | — | spam complaints reported by the caller (plan 015). A counter, not a gauge: the *rate* is what matters and a scraper computes that. What pauses sending is the count within a window, which lives where the timestamps are |
| `relay_sent_total` | counter | `outcome` | delivery attempts: `delivered`, `rejected`, `deferred`, and this service's own refusals — `suppressed`, `no_budget`, `no_mx` |
| `relay_queue_depth` | gauge | `state` | `ready`, `later`, `dead`. A gauge because the question is whether anything is stuck *now*, and `dead` above zero always wants a person |
| `verify_probe_blocked_total` | counter | `reason` | probes declined: `guarded`, `no_budget`, `paused`, `policy_stop` |
| `verify_catch_all_probes_total` | counter | `outcome` | how each domain's catch-all question was settled (plan 029): `skipped_no_accept` (no real `250` in the session — nothing to qualify), `clean_after_1`, `catch_all`, `randomiser`, `audit_full` (the audit sample's full sequence of 3), `unanswered` (the session died or budget was refused mid-sequence). Bounded. A rising `randomiser` or `audit_full` share with a non-clean result means hosts the early stop could misread |
| `verify_pause_events_total` | counter | `mx_host` | the pacer standing a pace key down at the floor of its band — the label is the **pace key** (plan 026): `@microsoft-eop` for every EOP tenant |
| `verify_rate_per_sec` | gauge | `mx_host` | rate the AIMD loop has settled on |
| `verify_concurrency` | gauge | `mx_host` | concurrency it has settled on — since plan 028 also the number of sessions the pace key may hold open at once |
| `verify_inflight` | gauge | `mx_host` | session leases **this process** holds for the pace key (plan 028); the authoritative count across nodes is `ZCARD rt:mx:<pace_key>:inflight`. Bounded like the other per-key gauges |
| `verify_lease_waits_total` | counter | `outcome` | how a session got its lease (plan 028): `immediate`, `granted_after_wait`, `timed_out` (past `pacer.lease_wait` — the addresses came back `paused` with a retry hint). A rising `timed_out` share means a family's single connection is the bottleneck |
| `verify_tls_sessions_total` | counter | `outcome` | how each SMTP session was encrypted (plan 031): `none` (not offered, or `probe.starttls: off`), `verified`, `unverified` (upgraded, certificate recorded as untrusted or not naming the MX — never refused), `fallback` (offered, refused or handshake failed, answered in plaintext), `skipped` (host remembered as TLS-broken — `mx:<host>:tls_broken` — plaintext without asking), `failed` (handshake failed and the plaintext retry failed too: class `tls_failed`). A failed handshake that the retry answers counts once, as `fallback`; the abandoned attempt is not counted |
| `verify_mx_state` | gauge | `mx_host`, `state` | 1 for the MX's current state |
| `verify_request_duration_seconds` | histogram | — | end-to-end `POST /probe` |
| `verify_tracked_mx` | gauge | — | pace keys the pacer holds state for (a family counts once) — **the cardinality canary** |
| `go_goroutines` | gauge | — | a service that must not leak them should say how many it has |

| `verify_key_stood_down` | gauge | `mx_host` (pace key) | 1 while a key is stood down for refusals of us (plan 032), 0 once it came back; only keys stood down since start appear. **Data Scout's `deploy/probe-standing.py` reads this exact line** and the hourly host check mails it |
| `verify_refusals_of_us_total` | counter | `mx_host` | refusals of us that count toward a stand-down: a family key is its own label, every lone host shares `host` — bounded by construction |
| `verify_session_domains` | histogram | `le` (1, 2, 3, 5, 10, 20, 50) | domains asked about per SMTP session that reached a server (plan 033). `1` is every ungrouped session; the rest is how much grouping happens |
| `verify_multi_domain_fallbacks_total` | counter | `family` | a family switched to one domain per session on this node after a receiver refused a foreign domain as relay (plan 033). At most once per family per process; bounded by `families.json` |
| `ip_health_policy_hosts` | gauge | — | distinct MX hosts that refused our client in the last hour (the `iphealth` count that had no reader until plan 032) |
| `ip_health_listed` | gauge | `ip`, `list` | 1 if this sending address is on the named blocklist |

`ip_health_listed` appears only once a check has run. Absent *altogether* means checking is off —
which is the default, and deliberately so: without a resolver that can answer DNSBL queries,
checking stays disabled rather than trusting the host's stub.

**A single zone missing from the series is a different fact, and since plan 021 it is a readable
one: that zone is not covered.** A zone kept with a caveat (plan 023 — it lists RFC 5782's clean
point) carries its series like any other; only a *dropped* zone is missing. The self-test runs per
zone and keeps only the zones that pass, so
a configured zone that cannot answer — a lapsed subscription key, a rename — drops out and is logged
at startup (`blocklist zone dropped`) rather than disabling the rest. Read the label set as *what is
actually being watched*: on 2026-09-12 the node reported `burned: false` with two clean series while
a receiving server was refusing it by name on a third zone nobody was querying.

So `ip_health_listed == 0` on the zones present never means "this IP is in good standing", only
"clean on these". Compare the series present against the configured `ip_health.zones`.

## Cardinality

Four metrics are labelled by `mx_host`, which arrives in the request. Since plan 026 the value of that label is the **pace key**, not the raw host: every tenant of a family (`@microsoft-eop`, `@google`, …) is one series, so day 11's 1,005 hosts would be a few hundred keys. The `smtp_reply` log line carries both `mx_host` and `pace_key`. Every one of them is bounded
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
