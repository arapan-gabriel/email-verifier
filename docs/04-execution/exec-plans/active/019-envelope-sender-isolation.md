# Plan 019 — envelope sender isolation

**Status:** Active
**Phase:** B
**Depends on:** 013

## Goal

Move the verification envelope sender from `verify@datascoutmail.com` to
`verify@probe.datascoutmail.com`, so probing reputation stops accumulating on the root domain
that carries the real sending identity. The identity split in `ARCHITECTURE.md` §"Sender
identity" becomes live rather than aspirational.

## Context

Plan 013 shipped the split as a table and could not switch it on: `probe.datascoutmail.com`
carried only its SPF TXT, with no MX and no A. A receiver doing sender-domain verification looks
up MX (then A) for the envelope sender's domain, finds nothing, and answers `554 5.1.8` — measured
against `gammait.net` in plan 001. That is a rejection of *us*, so under invariant 1 every such
probe classes `unknown` and the tier silently returns nothing. `mail_from` therefore stayed on the
root, and 013 recorded the missing MX as a deployment task.

Meanwhile the root acquired DKIM selectors `s1` and `cf2024-1`: it is now armed to send real mail,
and until this change probing and sending share one reputation.

**The blocker was one DNS record, and only the existence half was ever missing.** Re-measured
2026-09-10 *before* the record existed: `route1.mx.cloudflare.net` already answered
`250 2.1.0 Ok` to `RCPT TO:<verify@probe.datascoutmail.com>`. Cloudflare Email Routing accepts the
sub-domain at `RCPT` whether or not it is pointed at, so the callout half had been passing the
whole time. What failed was the MX lookup that precedes it.

## Design

No code changes. `probe.helo` and `probe.mail_from` are already config, already validated at
startup (`internal/config/config.go:437`), and already overridable per request
(`internal/api/probe.go:33`). This plan is a DNS record, a config value, and the documents that
recorded the blocker.

Zone `datascoutmail.com`, three records mirroring the root's routers and priorities:

```
probe.datascoutmail.com.  MX  22  route3.mx.cloudflare.net.
probe.datascoutmail.com.  MX  60  route1.mx.cloudflare.net.
probe.datascoutmail.com.  MX  84  route2.mx.cloudflare.net.
```

An A record is deliberately **not** added. RFC 5321 falls back to A only when no MX exists; an A
on `probe.` would name the probe host itself as its own implicit MX, and inbound `:25` there is
shut. That is worse than the state this plan fixes, because the callout would then reach a closed
port instead of a router that answers.

SPF on `probe.` is unchanged and stays `-all` with the single `ip4:92.222.87.97` — it is checked
against the *connecting* IP during the probe, so it must name the node and nothing else.

## Tasks

- [x] Add the three MX records to the Cloudflare zone
- [x] `config/verifierd.yaml` — `mail_from: "verify@probe.datascoutmail.com"`; replace the comment
      recording the blocker with what is now true
- [x] `docs/06-generated/api.md` — the `mail_from` example carried the fallback value
- [x] `docs/03-engineering/operations/dns.md` — record the deployed state against the shape
- [x] `docs/04-execution/exec-plans/completed/013-deployment.md` — append the resolution, dated;
      the original measurement stays as written
- [x] `docs/08-decisions/changelog.md` — entry
- [x] Data Scout `exec-plans/active/073-ops-smtp-egress-relay.md` — close its open item
- [ ] Deploy and run the manual-test gate below

- [x] `cmd/verifierd/main.go` — log `mail_from` at startup (see deviation below)

No test changes in `internal/`: nothing there moves, and the value was never asserted in a test —
it is host identity, not behaviour.

## Definition of Done

- [ ] **Manual-test gate:** from the node, a live probe with the new envelope sender against a
      receiver that verifies the sender domain returns a verdict about the *recipient* — never
      `554 5.1.8`, and never `unknown` on account of us. Recorded here with the reply.
- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` confirmed
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status Complete, moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

**Pre-deploy verification, from a laptop rather than the node.** `gammait.net`
(`mx1.privateemail.com`, the strict receiver from plan 001) was asked with both envelope senders,
2026-09-10:

```
verify@probe.datascoutmail.com   MAIL FROM -> 250 2.1.0 Ok
                                 RCPT      -> 450 4.1.1 <postmaster@gammait.net> ... unverified address
verify@datascoutmail.com         MAIL FROM -> 250 2.1.0 Ok
                                 RCPT      -> 450 4.1.1 <postmaster@gammait.net> ... unverified address
```

`554 5.1.8` is gone and the two senders are indistinguishable; the `4.1.1` is about the recipient.
**This is not the gate.** It ran from an address outside `probe.`'s SPF, so a receiver that checked
SPF would have failed it for a reason the production path will not have — it establishes that
sender-domain verification now passes, not that the production session does. The gate above must
run from `92.222.87.97`.

**Deviation: one line of code after all.** The plan said "no code changes", and the startup log
proved that wrong while the deploy commands were being written. It carried `helo` and `source_ip`
but not `mail_from` — so the value this entire plan exists to change was the one piece of deployed
identity a journal could not answer, and confirming a deploy meant reading the file on the host.
`cmd/verifierd/main.go` now logs it beside the other two. Same argument as plan 018 one layer down:
identity that cannot be read back is not deployed identity, it is a hope about a file.

**Deferred, not solved: bounces to this sender are discarded.** Cloudflare Email Routing answers
`250` for the sub-domain without a route behind it, so anything addressed to
`verify@probe.datascoutmail.com` is accepted and dropped. Acceptable while verification is the only
consumer — the prober never sends `DATA`, so there is nothing to bounce, and the address exists to
satisfy callouts. Phase C changes that: plan 015 (bounce handling) needs a real mailbox, and its
sender is `noreply@<root>`, not this one. Recorded in `tech-debt.md`.
