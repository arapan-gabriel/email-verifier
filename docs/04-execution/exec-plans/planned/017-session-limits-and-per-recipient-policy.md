# Plan 017 — session-limits-and-per-recipient-policy

**Status:** Planned
**Phase:** B
**Depends on:** 001 (the session), 005 (per-server memory), 007 (policy-stop), 009 (the metrics that found this)

## Goal

Teach this service the difference between *"you are going too fast"* and *"this session is full"*,
and between *"we refuse you"* and *"we refuse this recipient"*. Both distinctions are currently
collapsed, and both collapses hurt the same provider: Microsoft 365.

## Context

Found 2026-09-10, from Data Scout's live cut-over (its plan `073`, manual test step 6). The finder
cannot confirm an address on an M365 tenant even though plain verification of a known address on the
same tenant returns `250 2.1.5` and scores 100. M365 defeats a candidate ladder twice, and both
halves land on this service:

1. **The second recipient in a session answers `452 4.5.3 Too many recipients`** with a 900-second
   hint. Verified against our own classifier: that is `ClassThrottled`, and `Class.IsThrottle()` is
   true. So `observe()` tells the pacer the *rate* is wrong — when the truth is the *session* is
   full — and the `RCPT` loop moves to the next recipient **on the same connection**, which answers
   the same thing. A batch of six yields five throttle observations, halves the Microsoft rate five
   times, and returns five addresses as `unknown` that a fresh connection would have answered.

2. **Any recipient M365 will not confirm answers `550 5.4.1 Access denied`** — its
   directory-based edge blocking, worded so as not to confirm or deny. It is classed `ClassPolicy`,
   correctly, and never `invalid` (invariant 1 holds). Two consequences, and the second was missed
   at first: policy-stop counts *consecutive* policy replies, so a finder's ladder of guesses
   abandons its session before reaching the right candidate; and in plain verification an M365
   address that does not exist is indistinguishable from one the server merely will not discuss.

Neither reply is misclassified. The gap is that the service has **no notion of a per-MX limit on
recipients per session**, and no way to tell the policy-stop counter that a rejection was about the
recipient rather than about us.

**Corrected 2026-09-10 by measurement, and the correction changes which half matters.** The first
draft of this plan led with the `452` and said it was "already happening in production" across the
warm-up. It is not. Probing two unknown recipients at an M365 tenant we own showed
`550 5.4.1 Access denied` on the **first** recipient, not `452` on the second:

```
zz-probe-a-…@…   class=policy  550 5.4.1 Recipient address rejected: Access denied
zz-probe-b-…@…   class=policy  550 5.4.1 Recipient address rejected: Access denied
```

M365 answers `5.4.1` to *any* recipient it will not confirm, from the first, in an ordinary
single-recipient session. The `452 4.5.3` appears when a session carries many recipients — which is
the finder's candidate ladder, not verification, because Data Scout groups by domain and the
warm-up pool yields about one address per domain.

So **(2) is the half that bites today** and (1) is the half that bites the finder. Both are still
worth fixing; the ordering of the work changes.

**And (2) has a consequence nobody had written down: the warm-up's governing metric is blind on a
third of its pool.** Microsoft fronts 811 of the 2,586 domains in that pool — 31%. A dead address at
an M365 tenant answers `5.4.1`, classes `ClassPolicy`, reaches Data Scout as `block: true`, and is
scored `valid`/50 — **never `invalid`**. That is correct, and invariant 1 requires it. But the ladder
advances on the invalid share, so on 31% of the list that share cannot move, while the receiving
servers still see us asking about mailboxes that do not exist.

Invariants in play: **4** (the budget belongs to the MX — a reconnect is a new session and must take
a token), **6** (a limit misattributed to rate calibrates the wrong thing — the same reasoning that
keeps `ClassPolicy` out of the throttle signal), and **1** (nothing here may turn a non-answer into
`invalid`).

## Design

### A session limit is not a rate limit

`452 4.5.3` is matched on the **enhanced code**, not on `452` alone — `452 4.2.2` is a full mailbox,
which genuinely is about the recipient and must keep its current meaning. The phrase
"too many recipients" is a secondary signal for servers that omit the enhanced code.

When it is seen on a `RCPT`:

- The recipient it was seen on is **not answered** — it goes back to the queue, not into `out`.
- The session ends there. The remaining recipients of the chunk are re-queued.
- **The pacer is not told anything.** `observe()` skips it: slowing down does not enlarge a
  session, and if it counted, one M365 batch would calibrate Microsoft to the floor — the same
  failure invariant 6 exists to prevent, arriving through a different door.
- `Probe` opens a **new session** for the remainder, taking a token first (invariant 4). Reconnects
  are bounded per call, so a server answering `4.5.3` to the very first recipient of a fresh session
  cannot become a loop; at that point the reply *is* about us, and the remainder returns
  `ClassThrottled` with the server's hint, as today.

### The limit is a property of the server, and worth remembering

`k` — the number of recipients that were accepted before the refusal — is stored the way the
randomiser verdict already is (`internal/mxprofile`, plan 005): a per-host key with a TTL, so it
applies to every domain behind that host and survives a restart. `mx:<host>:max_rcpt`, alongside
`mx:<host>:randomiser`. Subsequent chunking for that host uses `min(configured, remembered)`, so the
second batch never re-learns the lesson at the cost of another wasted connection.

A remembered `k` is a floor of 1, never 0: a host that accepts no recipients at all is not a session
limit, it is a refusal, and must not be silently retried one recipient at a time forever.

### Per-recipient policy versus policy about us — **decision required**

Option 1 keeps policy-stop as it is and gives the *caller* the control: `POST /probe` accepts an
optional lower `policy_stop` for a request whose shape is a candidate ladder. Simple, no
classification risk, and it puts the choice with the code that knows what it is doing — Data Scout's
finder knows its candidates are guesses; this service cannot.

Option 2 splits `ClassPolicy` into "about the connection" (`5.7.x` before `MAIL FROM`, PTR, SPF,
blocklist wording) and "about this recipient" (`5.4.1 Access denied` on a `RCPT` after a good
`MAIL FROM`), counting only the first toward policy-stop.

**Recommended: option 1**, and it is the conservative one. Option 2 asks this service to decide
which rejections are "really" about us, and being wrong in one direction means probing on through a
server that is refusing the client — the exact behaviour that gets an IP blocked, and the thing
policy-stop was written to prevent. The measurement M365 gives us is not strong enough to justify
that; a per-request ceiling costs nothing and is reversible.

Recording it as a decision rather than picking it silently, because it changes the API.

## Tasks

- [ ] `classify.go`: recognise a session-capacity refusal by enhanced code (`4.5.3`), with the
      phrase as a secondary signal; `4.2.2` keeps its current meaning
- [ ] Internal only — it must **not** become a `Class` on the wire while a retry can still answer
      the recipient. It surfaces as a verdict only when the reconnect budget is spent
- [ ] `prober`: end the session, re-queue the refused recipient and the remainder, open a fresh
      session with a fresh token (invariant 4); bound reconnects per `Probe` call
- [ ] `observe()` does not report a session-capacity refusal to the pacer (invariant 6's reasoning)
- [ ] `mxprofile`: remember `k` per host with a TTL, floor 1; chunk at `min(configured, remembered)`
- [ ] Config: the reconnect bound and the memory's TTL
- [ ] Decide per-recipient policy (option 1 or 2 above); implement the chosen one
- [ ] `mxsim`: a profile that answers `452 4.5.3` after the *n*-th recipient — the simulator has no
      such behaviour today, which is why nothing caught this before production did
- [ ] Metrics: a counter for session-capacity refusals and one for reconnects, so the next
      provider that does this is visible before a manual test finds it
- [ ] Tests: the limit is learnt, remembered, and applied to the next batch; the pacer's rate is
      unchanged across a limit event; a first-recipient refusal does not loop; every re-queued
      address ends with a real answer or `unknown`, never `invalid`
- [ ] Update `docs/06-generated/api.md`, `redis-contract.md`, `metrics.md`,
      `patterns/smtp-classification.md`, `aimd-pacing.md`, changelog

## Definition of Done

- [ ] Against `mxsim`: a host capping at *n* recipients answers every address in a batch of
      `n × 3`, in three sessions, with no address left `unknown`
- [ ] **The per-MX rate is unchanged across a session-limit event** — asserted, because treating it
      as throttling is the defect this plan exists to remove
- [ ] The learnt limit survives a restart and is applied to the first batch after it
- [ ] A host refusing the first recipient of a fresh session does not reconnect in a loop, and the
      remainder returns `unknown` with the server's own words
- [ ] **A live M365 tenant: a batch larger than the limit is fully answered**, recorded here with
      the session count. This is the check that closes Data Scout's manual test step 6
- [ ] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [ ] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

**Why this is not folded into plan 008.** 008 is the cut-over, and the cut-over has happened. This
is a defect the cut-over *found*, which is what a rollout is for; putting it in 008 would leave that
plan open for weeks and hide the finding inside it.

**Why the simulator gets a task.** `mxsim` has no recipients-per-session behaviour, so no test in
this repository could have failed. Production found it, at 2,000/day, in the one manual test that
needed a real provider. The simulator gaining that profile is the difference between this class of
bug being caught here or at 30,000/day.

**The finder's ladder is the hard case, and it is Data Scout's call too.** Even with a session limit
learnt, six candidates at one recipient per session is six connections for one answer. Their
`find_max_candidates` is 6. If M365's real limit turns out to be 1, the honest options are to spend
six connections, to shorten the ladder, or to accept `confirmed: false` there — recorded in their
`tech-debt.md` with the same three. This plan makes the first *possible*; it does not decide that it
is *wise*.
