# Plan 018 — explain-a-verdict

**Status:** Complete (2026-09-10)
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

- [x] `internal/prober`: an optional `OnReply` hook, called for classes other than `valid`/`invalid`
- [x] `cmd/verifierd`: wire it to `slog` at `info` — `mx_host`, `class`, `smtp_code`,
      `enhanced_code`, truncated reply. The off switch returns a **nil** hook rather than a
      discarding one, so the cost when disabled is one nil test per result, not a call
- [x] Redact — **strip, then truncate.** The plan had the order backwards and implementing it
      showed why: cutting `550 5.1.1 <john.smith@example.com> unknown` at 26 characters leaves
      `<john.smith@examp`, which no longer looks like an address to any matcher and still carries
      the whole local part. Tokens containing `@` are replaced whole, so a mangled address goes too
- [x] Tests: a line is emitted for `policy`/`throttled`/`deferred`; none for `valid`/`invalid`;
      **no local part appears in any of them**, including five shapes of quoted address, and the
      strip-before-truncate order is asserted rather than only its outcome
- [x] Config: `log.reply_max_chars` (200) and `log.replies` (on)
- [x] Update `docs/06-generated/metrics.md` (says why the MX label is deliberately absent),
      `operations/observability.md`, changelog
- [x] **Data Scout (cross-repo):** `smtp_code`, `enhanced_code` and `reply` on `ProbeResult`, read
      in `_result_of`, stored in `signals` for `block` and `invalid` rows, **and exposed on the API
      response** — their own contract test caught that half, which is what it exists for; two tests
      that a blocked row explains itself and an ordinary one carries nothing

## Definition of Done

- [x] A `block` verdict can be explained from the journal, naming the MX host and quoting the
      server — demonstrated live on `futurefertility.com`, one of the four blocked domains:
      `{"msg":"smtp_reply","mx_host":"futurefertility-com.mail.protection.outlook.com",`
      `"class":"policy","smtp_code":550,"enhanced_code":"5.4.1","reply":"550 5.4.1 Recipient address`
      `rejected: Access denied. For more information see https://aka.ms/EXOSmtpErrors [...]"}`
- [x] The same verdict can be explained from its `email_verifications` row alone, with no access
      to the probe node — `smtp_code`, `enhanced_code`, `smtp_reply` in `signals`, tested
- [x] **No log line at `info` contains a local part**, including when the reply quotes it —
      tested, and checked live on the node: the only `@` in the journal is our own DMARC `rua`
      address, printed by the preflight from a public DNS record
- [x] `verify_smtp_replies_total` still has bounded cardinality — unchanged, and `metrics.md` now
      says why it will stay that way
- [x] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added in both repositories
- [x] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Results (2026-09-10)

Deployed through the release script and demonstrated against a live Microsoft 365 tenant we own. The
verdict that could not be explained this morning now reads:

```
{"msg":"smtp_reply","mx_host":"futurefertility-com.mail.protection.outlook.com","class":"policy",
 "smtp_code":550,"enhanced_code":"5.4.1","reply":"550 5.4.1 Recipient address rejected: Access
 denied. For more information see https://aka.ms/EXOSmtpErrors [...]"}
```

Their side: 848 unit tests green, `mypy` over 187 files, `ruff` and its formatter clean. Ours: 14
packages with `-race`, `vet`/`gofmt`/`golangci-lint` clean.

**Their contract test earned its place.** Adding the three fields to `signals` failed
`test_every_stored_signal_is_exposed` immediately — stored in the row, dropped from the API
response, which is the "recorded and invisible" state that test was written after `randomiser` and
`source_ip` spent a fortnight in exactly it. Two lines of schema, caught before the commit rather
than by someone wondering why a field is always absent.

**One design correction found by implementing.** The plan said "truncate, then strip anything
address-shaped". That is backwards and quietly so: the cut destroys the shape redaction matches on
while leaving the local part behind. Stripping first is now the code, and the test asserts the
*order* rather than only the outcome — an outcome test would pass on the broken version for any
input short enough not to be cut.

**A note on what remains unmeasured.** The point of this plan was to make the warm-up's stop rule
applicable. It now is, for verdicts produced from here on. The three `block` rows from day 2 are
**not** retroactively explainable — nothing recorded them. Day 3 is the first that can be judged on
evidence rather than inference.

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
