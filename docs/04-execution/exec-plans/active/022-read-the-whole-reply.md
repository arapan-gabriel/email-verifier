# Plan 022 — Read the whole reply: a multi-line refusal of us is not a missing mailbox

**Status:** Active — code and tests done 2026-09-15 and **deployed the same day, 13:23 UTC**
(sha `307f4e1`, `deployed and healthy`). **One item remains: the manual gate below.** The node's
zone list turned out to need a code fix first — `rbl.your-server.de` lists `127.0.0.1` and our own
`selfTestZone` dropped it — and that, with the node step, moved to **plan 023**
**Phase:** B
**Depends on:** 018, 021

## Goal

Return, log and classify every line of an SMTP reply, not only the last, so that a receiving
server refusing *us* in a multi-line reply is read as `policy` and reaches Data Scout's stop rule
with its reason intact.

## Context

Warm-up ladder day 5, 2026-09-15. Eight domains on Hetzner's managed mail (MX `*.your-server.de`)
answered `451 Ihrem Serveranbieter erfahren.`, and on a greylist recheck one answered
`550 Ihrem Serveranbieter erfahren.` — which `Classify` called **invalid**. A manual session from the
node with `swaks` showed the reply whole:

```
550-Unfortunately we cannot currently accept your e-mail due to the amount of
550-spam we are receiving from your server. Please check
550-https://rbl.your-server.de/?ip=92.222.87.97 for further details or contact
550-your server provider. / Leider koennen wir Ihre E-Mails aufgrund der
550-Spamaufkommens von Ihrem Server momentan nicht annehmen. Weitere Details
550-können Sie unter https://rbl.your-server.de/?ip=92.222.87.97 bzw. von
550 Ihrem Serveranbieter erfahren.
```

`readReply` walked the continuation lines and kept only the last (`last = line`). So the classifier,
the journal (plan 018) and Data Scout's `signals.smtp_reply` all saw the sign-off, never the reason:
a blocklist refusal naming our IP arrived as an address verdict — **invariant 1 broken** — and the
ladder's stop rule, which reads that text for a list or our IP, had nothing to match. The same defect
discards the EHLO capability list, already recorded under the STARTTLS item in `tech-debt.md`.

Even whole, the reply would still have been `invalid`: it carries no RFC 3463 code, and none of
`senderHints` matched its wording.

## Design

- `readReply` (`internal/prober/classify.go`) keeps every line, joined by `\n`, each with its own
  `NNN-` prefix, so the text is what the server sent. It stops *keeping* at `maxReplyBytes` (4096)
  but still *reads* to the final line, so the session stays in step with the server.
- `senderHints` gains the prose a receiving server's own blocklist uses: `amount of spam`,
  `spam we are receiving`, `spamaufkommen`, `rbl.`, `dnsbl`, `blocklist`, `block list`,
  `black list`. `rbl.` rather than `rbl`, which matches `marble`.
- Unchanged on purpose: `EnhancedCode` anchors on the start of the text, which is the first line; a
  4xx still classes `deferred`/`throttled` only (see Notes).
- No new endpoint, Redis key or metric. `reply` in `POST /probe` changes meaning from "last line" to
  "the whole reply" — documented in `docs/06-generated/api.md`.

## Tasks

- [x] `readReply` keeps the whole reply, bounded
- [x] Blocklist prose in `senderHints`
- [x] Tests: the Hetzner reply read whole and classed `policy`; the reader stops exactly at the
      reply's end; a 50 KB reply is consumed and kept to the bound; the same refusal end to end at
      `RCPT` through `scriptedMX`; `marble` is not an RBL
- [x] Docs: `api.md`, `smtp-classification.md`, `tech-debt.md`, `changelog.md`, `ROADMAP.md`
- [x] Deploy through `deploy.yml` — 2026-09-15 13:23 UTC, run `34974628333`; `verifierd` active and
      the installed binary's hash matching the artefact CI tested
- [ ] ~~Add `rbl.your-server.de` to `VERIFIERD_IP_HEALTH_ZONES` on the node~~ — **blocked 2026-09-16,
      and not by the node.** Re-measured before writing the env file: `2.0.0.127` → `127.0.0.2`
      (TXT `"Local RBL"`, a static test point) and `1.0.0.127` → **`127.0.0.2`** as well (TXT
      `"Last seen 2026-09-16 08:30:03"` — a live listing of `127.0.0.1`, auto-populated from what
      receiving servers report). `1.2.0.192` and `5.5.5.5` answer nothing, so the zone is not a stub;
      it simply lists the address RFC 5782 says a list must never carry. `selfTestZone` would drop it
      on every start and log an error, so adding it now buys an error line and no coverage. The
      15.09 note above recorded the pair as passing; it does not today. Fix and reasoning in
      `tech-debt.md` — the clean-point assertion needed a second, un-listable point first. **Moved to
      plan 023**, which makes that change and owns the node step; this plan does not wait for it

## Definition of Done

- [ ] Manual gate: after the deploy, one probe to a Hetzner-hosted recipient returns a `reply`
      with every line, and the journal line for a `policy` class carries the reason, redacted
- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean (0 issues)
- [x] `pr-checklist.md`: SSRF guard and fail-closed untouched; "us ≠ address" is the point of the
      change and is tested
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **Deviation from `ds-smtp-retry`**, which carries the same last-line reader.
- **A 4xx blocklist refusal still classes `deferred`** and gets a greylist-style retry hint. That
  retry is what turned today's `451` into a `550`, and each retry is more traffic at a server that
  has us listed. Not changed here, because moving 4xx into `policy` changes retry, policy-stop and
  IP-health counting at once; recorded in `tech-debt.md`.
- Data Scout bounds `smtp_reply` at 300 characters (`_REPLY_MAX`). Hetzner names the list and our IP
  in the first ~220, so the stop rule sees them; a longer preamble would not. Recorded on that side.
