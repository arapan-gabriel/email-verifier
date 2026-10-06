# Plan 035 — A full mailbox said in words is not `invalid`

**Status:** Deployed 2026-10-06 14:50 UTC (`8dd8e61`, deploy run 37482278671) — waiting for a live over-quota reply to confirm (written 2026-10-06)
**Phase:** B
**Depends on:** 030 (refusals of us classed `policy`)

## Goal

A receiver that says the mailbox is full **in words** — without (or besides) the `5.2.2` code — is
answering "this mailbox exists": the verdict is `valid`, as it already is for `5.2.2`. And a quota on
**our sending** said in words is a refusal of us (`policy`), never a verdict on the address.

## Context

- Data Scout's warm-up day 25 (2026-10-05): one bare `550 Mailbox over quota` reply — no enhanced code —
  fell through `classifyPermanent` to its default and was classed `invalid` (Data Scout tech debt
  *"`550 Mailbox over quota` is classed `invalid`"*). The warm-up gate already treated the wording as
  benign; customers' rows did not.
- The same fall-through classed a wordy rate refusal (`550 Quota exceeded: too many messages per hour
  from your IP`) as `invalid` — invariant 1 says a rejection of us is never a rejection of the address.

## Design

`overQuotaRe` (over quota, quota exceeded, exceeded storage/quota, mailbox full, storage allocation, disk
quota, insufficient storage, mailbox size limit) checked in `classifyPermanent` after the sender-side
rules: with `sendingQuotaRe` (send/sent/outbound/rate/per hour…) and no recipient code (none or 5.7.x) →
`ClassPolicy`; without it and no code or 5.2.x → `ClassValid`. A `5.1.x` code still wins (it names the
recipient's address as wrong).

## Tasks

- [x] `internal/prober/classify.go` — the two regexes and the rule
- [x] `classify_test.go` — bare `550 Mailbox over quota`, `552 … mailbox full`, `552 5.2.2 … storage
      allocation` → valid; `550 5.7.1 Daily sending quota exceeded` and a wordy per-hour quota → policy
- [x] `docs/03-engineering/patterns/smtp-classification.md`; changelog
- [~] Deploy; a live over-quota reply classed `valid` when one next appears — **deployed 2026-10-06 14:50 UTC**; the live reply is waited for (one in 2,939 on day 25)

## Definition of Done

- [x] `go test -race -count=1 ./...` (with Redis), vet, gofmt, golangci-lint, coverage gate — 2026-10-06
- [x] `pr-checklist.md` — us ≠ address: a sending quota is `policy`; no SSRF / fail-closed surface touched
- [ ] Deployed; Status Complete, moved to `completed/`, `ROADMAP.md`
