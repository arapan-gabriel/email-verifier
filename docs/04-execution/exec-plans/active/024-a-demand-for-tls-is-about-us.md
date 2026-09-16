# Plan 024 — A demand for TLS is about us, not about the mailbox

**Status:** Active — started 2026-09-16
**Phase:** B
**Depends on:** 018, 022

## Goal

Class a 5xx that refuses the *session* for want of encryption as `policy`, so an address at a
TLS-only MX is `unknown` rather than deleted — and so the warm-up ladder's gate stops counting our
own missing STARTTLS as a dead mailbox.

## Context

Warm-up ladder day 6, 2026-09-16, part 12:

```
stewe.de   550 A TLS connection is required
```

No RFC 3463 code, and none of `senderHints` matches "a TLS connection is required", so
`classifyPermanent` falls through its last branch to **`ClassInvalid`** — invariant 1 broken, the
same shape as day 5's Hetzner refusal that plan 022 fixed for blocklist prose.

Two costs, and the second is the one that compounds:

- **A live address is recorded dead.** Data Scout stores `invalid` for a mailbox nobody asked about,
  because the session never got past the TLS demand.
- **It inflates the ladder's gate.** Day 6 read 7 invalid of 354 answered (1.98%); one of the seven
  is this row. The gate decides whether the next rung is safe, so a misread makes the source look
  dirtier than it is — and on 2026-09-16 that is exactly what stopped a clean day: the runner's
  counter tripped at 4.5% over a partial denominator with this row in it.

**This is the known STARTTLS gap seen from the other side.** `tech-debt.md` records that the prober
has no STARTTLS step, so an MX that requires it is permanently unanswerable — day 3 measured
`530 5.7.0 STARTTLS is mandatory`, and day 5 six more of that family. Those carried `5.7.x` and were
classed `policy` correctly. The ones with **no enhanced code** are the gap this plan closes. It does
not add STARTTLS; it stops the absence from producing a verdict about somebody's mailbox.

## Design

`senderHints` gains the wording a server uses when it refuses the session for encryption:
`starttls`, `tls connection is required`, `tls is required`, `tls required`, `requires tls`,
`session encryption is required`, `encryption is required`.

- Matched only when nothing in `mailboxHints` also matches, which is the existing rule — a reply
  that names both stays about the recipient.
- `starttls` as a bare token deliberately: RFC 3207's own wording is "must issue a STARTTLS command
  first", and no mailbox rejection contains it.
- Nothing else changes. Replies that already carry `5.7.x` were already `policy`; this is only about
  the no-code fall-through.

## Tasks

- [x] TLS prose in `senderHints`
- [x] Tests: the measured `550 A TLS connection is required` classes `policy`; RFC 3207's "must issue
      a STARTTLS command first" too; a reply naming both TLS and a missing user stays `invalid`;
      `5.7.0 STARTTLS is mandatory` is unchanged
- [x] Docs: `smtp-classification.md`, `tech-debt.md` (the STARTTLS item gains this half),
      `changelog.md`, `ROADMAP.md`

## Definition of Done

- [x] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean (0 issues)
- [x] `pr-checklist.md`: this is a verdict path, and the item it turns on is invariant 1 — "us ≠
      address" — which the tests state directly
- [ ] Deployed, and the next ladder day carrying a TLS-only MX records `policy`, not `invalid`
- [ ] Status Complete, moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

- **The rows already stored are left as they are.** Day 5 set the precedent with
  `office@unterscheider-bestattung.at`: the warm-up README records the misread rather than editing
  the database by hand, because the record of what the engine said is what makes a gate auditable.
- **Not fixed here:** the prober still cannot do STARTTLS, so those addresses stay unverifiable —
  `unknown`, which is the honest answer. The tech-debt item keeps the fix.
