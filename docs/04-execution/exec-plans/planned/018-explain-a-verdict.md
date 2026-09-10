# Plan 018 — explain-a-verdict

**Status:** Planned
**Phase:** B
**Depends on:** 009 (the logging and metrics this extends)
**Do this before 017**, and before the warm-up ladder cuts another day.

## Goal

Make a `block` verdict explainable an hour after it happened. Right now neither this service nor
Data Scout keeps what the server actually said, and the warm-up ladder's stop rule is written
against exactly that verdict.

## Context

Found 2026-09-10 while reading Data Scout's warm-up, on its second day. The ladder advances on the
share of `invalid` and holds on any movement in `block`, and `block` moved: **0 of 102 on day 1, 3
of 100 on day 2.**

Whether that matters is unanswerable with what is stored. Two of the four blocked domains are
Microsoft 365 tenants, where `550 5.4.1 Access denied` is the *normal* answer for a recipient the
tenant will not confirm — expected, harmless, and nothing to do with our reputation. The other two,
`tirol.gv.at` and `topparfuemerie.de`, run their own MX and might be the real thing. **Those two
cases require opposite responses — carry on, or stop the ladder — and nothing recorded distinguishes
them.**

What is missing on each side:

- **Here.** `internal/prober` produces `Reply`, `SMTPCode` and `EnhancedCode` on every `Result` and
  returns them over the API. It logs none of them: the journal holds request lines only
  (`msg=request`, path, status, duration). `/metrics` counts
  `verify_smtp_replies_total{code,class}`, which says *how many* `550`s were policy and never *from
  which host* or *saying what*.
- **Data Scout.** `ProbeResult` has no field for any of the three. The verifier returns `reply`,
  `smtp_code` and `enhanced_code`; `_result_of` reads `class`, `retry_after_seconds`, `randomiser`,
  `catch_all` and `connected`, and drops the rest. The verdict row records `block: true` and nothing
  about why.

So the one signal the rollout is steered by is the one signal that cannot be investigated. That is
the whole plan.

## Design

### What gets logged, and what must not be

At `info`, for every result whose class is **not** `valid` or `invalid` — the classes that are
already self-explanatory — one line carrying `mx_host`, `class`, `smtp_code`, `enhanced_code` and
the reply, truncated.

**The recipient's local part never appears** (plan 009's rule, and its test). The domain may: it is
in the MX host anyway and it is what makes a line searchable. The reply text is the server's own
words about a transaction, not personal data — but it is truncated, because some servers echo the
address they are refusing back into the message, and an untruncated reply would smuggle the local
part into a log line through the server's mouth rather than ours. Truncate, then strip anything
resembling an address from what is left; a test asserts both.

Volume is not a concern at this shape: these are the classes that are *not* the common case. Day 2
of the ladder produced three of them in a hundred.

### Carrying it to where the verdict lives

The log line explains an incident on the probe node. The verdict lives in Data Scout's
`email_verifications`, is read weeks later, and is what the ladder is judged on — so the same three
fields belong in `signals` for rows that are `block` or `invalid`. That is the cross-repo half:
three fields on `ProbeResult`, carried into `signals` where `source_ip` already goes.

Both halves are wanted. The log answers "what happened at 10:51"; the row answers "why is this
address `valid`/50 and not `invalid`", which is the question the ladder actually asks.

### A metric for the shape of it

`verify_smtp_replies_total{code,class}` gains `mx_host`… **no.** That is the cardinality mistake
plan 009 spent its effort bounding, and an MX label on a reply counter is unbounded by request
input. The log line is the per-host record; the metric stays aggregate. Recorded here because it is
the obvious next thought and it is wrong.

## Tasks

- [ ] `internal/prober`: an optional `OnReply` hook, called for classes other than `valid`/`invalid`
- [ ] `cmd/verifierd`: wire it to `slog` at `info` — `mx_host`, `class`, `smtp_code`,
      `enhanced_code`, truncated reply
- [ ] Redact: truncate, then strip anything address-shaped from the remainder
- [ ] Tests: a line is emitted for `policy`/`throttled`/`deferred`; none for `valid`/`invalid`; **no
      local part appears in any of them**, including when the server echoes the address back
- [ ] Config: the truncation length, and an off switch
- [ ] Update `docs/06-generated/metrics.md` (say why the MX label is deliberately absent),
      `operations/observability.md`, changelog
- [ ] **Data Scout (cross-repo):** `smtp_code`, `enhanced_code` and `reply` on `ProbeResult`, read in
      `_result_of`, stored in `signals` for `block` and `invalid` rows; a test that a blocked row can
      be explained from the row alone

## Definition of Done

- [ ] A `block` verdict from the warm-up can be explained from the journal, naming the MX host and
      quoting the server — demonstrated on one of the three from day 2
- [ ] The same verdict can be explained from its `email_verifications` row alone, with no access to
      the probe node
- [ ] **No log line at `info` contains a local part**, including when the reply quotes it — tested
- [ ] `verify_smtp_replies_total` still has bounded cardinality
- [ ] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added in both repositories
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

**Why this comes before 017.** 017 changes how policy replies are counted. Deciding that without
being able to read the replies would be guessing, and the ladder is spending real addresses at a
real provider while the guess stands.

**Why the ladder should hold at day 2 until this lands.** Its rule is "hold on any `block`
movement". `block` moved. The rule cannot be applied, because two of the movements are M365
behaving normally and two might not be — and there is no way to tell. Advancing anyway would be
following the letter of a stop rule whose evidence does not exist.

**A related asymmetry, recorded here and not fixed here.** On M365 tenants — 31% of that pool — a
dead address answers `5.4.1` and is scored `valid`/50, never `invalid`. So the metric the ladder
advances on cannot move on a third of the list, while the receiving servers still see the requests.
That is a property of the source and the provider, not a defect, but it means the invalid share is
an *understatement* of list quality by construction. Whoever reads the gate should know by how much.
