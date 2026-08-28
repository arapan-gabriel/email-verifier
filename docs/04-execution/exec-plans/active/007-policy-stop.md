# Plan 007 — policy-stop

**Status:** Active
**Phase:** A
**Depends on:** 001, 003

> Renamed from `007-bulk-verify-and-queue`, of which ADR-006 left exactly one behaviour. The bulk
> endpoint, the job, the group-by-MX runner and the results retrieval are all Data Scout's Celery
> task, which already owns chunking, progress, quota and the artifact. The transport-agnostic engine
> entrypoint the plan asked for is how `internal/prober` was built in the first place.

## Goal

Stop asking a server questions it has already refused to answer.

## Context

`ClassPolicy` is a permanent rejection **about the connecting client** — its IP, its rDNS, its HELO
— and not about the recipient. `550 5.7.25 Forward-confirmed reverse DNS failed` says nothing
whatsoever about whether a mailbox exists.

When a server decides that about us, it decides it for the whole session. Every remaining `RCPT` in
the batch will get the same answer, so continuing to send them:

- spends a token per recipient on a question whose answer is already known,
- adds nothing to the result — those addresses were going to come back unattempted either way,
- and keeps hammering a server that has just told us to go away, which is how a soft block hardens.

Fifty recipients in a batch means up to forty-nine pointless probes after the first refusal is
unambiguous.

## Design

- Count **consecutive** `ClassPolicy` replies within one session. At `probe.policy_stop` (default 5,
  `0` disables) stop the session and mark every remaining recipient
  `connected:false`, `class:policy`, `err: "not attempted: …"`.
- Consecutive, not cumulative: one policy reply among ordinary answers is a per-recipient quirk, not
  the server refusing the client. The counter resets on any non-policy reply.
- Catch-all probing is skipped once the stop trips. A server refusing us cannot tell us anything
  about which local parts exist.
- **Nothing here touches the pacer** (invariant 6). Slowing down does not grow a PTR record, and if
  policy counted as throttling one blocked IP would calibrate every provider to zero.

### Where the cross-request version lives

Stopping *within* a batch is this plan. Remembering that a server refuses us, and refusing to probe
it on the next request, is **plan 010** — that is the same signal as "our IP is burned", it belongs
with the IP-health state and the alert, and the right response there may well be to pause the node
rather than one server. Building a second, overlapping mechanism here would make the two disagree.

## Tasks

- [x] Consecutive-policy counter in the session; stop at the threshold
- [x] Remaining recipients marked `connected:false`, `class:policy`, with a reason
- [x] Catch-all probing skipped once tripped
- [x] `probe.policy_stop` in config, validated, `0` disables
- [x] Tests: trips at the threshold, resets on a non-policy reply, disabled at 0, pacer untouched
- [x] Update `api.md`, `smtp-classification.md`, changelog

## Definition of Done

- [x] A server answering every `RCPT` with `5.7.x` stops the session at the threshold; the rest are
      `connected:false` with a reason, and no further `RCPT` is sent
- [x] An isolated policy reply among ordinary answers does **not** stop the session
- [x] `policy_stop: 0` sends the whole batch
- [x] The pacer receives no throttle signal from any of it
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [ ] Status → Complete, moved to `completed/`, ROADMAP row updated — pending manual sign-off

## Results (2026-08-28)

Gate clean.

| Check | Result |
|---|---|
| 20 recipients, every reply `5.7.25`, threshold 5 | 5 probed, 15 `not attempted`, all 20 accounted for |
| Budget | 5 tokens spent, not 20 — stopping early stops spending |
| Pacer | zero throttle signals from any of it |
| Every third address answering normally | nothing skipped; the run never reaches the threshold |
| `policy_stop: 0` | whole batch sent, full budget spent |
| Catch-all probing after a stop | zero bogus probes |
| `policy_stop: 1` | refuses to boot |

## Notes / decisions / deviations

Five is the lab's default and is kept for that reason rather than because it was measured here. It
is deliberately not one: a single `5.7.x` can be a per-recipient policy (a distribution list that
rejects external senders, say), and stopping a fifty-recipient batch on the strength of one reply
would throw away forty-nine answers we could have had.
