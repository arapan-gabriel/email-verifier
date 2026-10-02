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
- **What the server said, for verdicts that need explaining** (plan 018). One `smtp_reply` line per
  result whose class is neither `valid` nor `invalid`, carrying `mx_host`, `class`, `smtp_code`,
  `enhanced_code` and the reply. Those two classes are skipped because a `250` or a clean `5.1.x` is
  the whole story and they are the common case — logging them would bury the rest. Everything else
  is a verdict somebody later has to explain: a `block` that moved a rollout's stop rule, a throttle
  that halved a rate. Off with `log.replies: false`; capped by `log.reply_max_chars`. Since plan
  026 each line also carries `pace_key`, and since plan 031 `tls` (the session's STARTTLS outcome).
- **When each question was asked** (plan 026). One `rcpt_paced` line per token the pacer granted
  — every `RCPT`, catch-all probes included — with `mx_host` and `pace_key`. It is the only record
  of the rate a receiving system actually saw, because `smtp_reply` skips the `250`s: a family's
  aggregate rate is read from these timestamps (`journalctl -u verifierd -o cat | grep rcpt_paced |
  grep '"pace_key":"@microsoft-eop"'`). Same switch as the reply log; no address, by construction.
- **How long a session waited for its connection** (plan 028). One `session_leased` line per session
  lease, with `mx_host`, `pace_key` and `lease_wait_ms` — the cost of one connection per receiving
  system, read per family. Same switch; no address.
- **No address at info level.** The domain and a count are enough to find a problem; the local part
  is the customer's data. Full SMTP transcripts stay at debug. A test asserts it.

  The reply is the one field where an address can arrive **through the server's mouth rather than
  ours** — many servers quote the recipient back when refusing it. So the prober redacts before the
  hook fires, not at the caller: any token containing an `@` becomes `<redacted>`, whole, because
  the goal is that nothing address-shaped survives rather than that well-formed addresses are
  recognised. **Redaction runs before truncation**, which is not the same as after: cutting
  `550 5.1.1 <john.smith@example.com> unknown` at 26 characters leaves `<john.smith@examp`, which no
  longer looks like an address to any matcher and still carries the whole local part. A test asserts
  the order, not just the outcome.
