# Pattern — SMTP reply → verdict

The single source of truth for turning a reply into `valid | invalid | risky | unknown`. Ported from
`ds-smtp-retry/ratecheck/internal/prober`. Lives in one place (`internal/prober`); nowhere else maps
a code to a verdict.

## The core rule (invariant 1)

**A rejection of *us* is never a rejection of the address.** Only a `5xx` on `RCPT` *after* a good
`MAIL FROM` may mean "no such mailbox". Everything else that fails — connection refusal, TLS error,
timeout, 4xx, a blanket 550 before `MAIL FROM` — is `unknown`.

## Read the enhanced code first (RFC 3463)

The `5.X.Y` enhanced status answers "who is this about" before the prose. Subject `X`:

| Reply | Verdict | About |
|---|---|---|
| `250` | valid *(risky on catch-all/randomiser)* | mailbox accepts |
| `550 5.1.1` NoSuchUser | invalid | recipient |
| `550 5.2.2` mailbox full | valid | mailbox exists |
| `452 4.2.2` over quota | unknown (retry) | recipient's box — **not our rate** |
| `450`/`451` greylisting | unknown (retry) | per-recipient, rate-independent |
| `550 5.7.x` / `554 …blocked` / reverse DNS | **policy** → unknown | **our IP** |
| `421 4.7.x` / unusual rate | throttled → unknown (back off) | our rate |

## Two classes that look temporary but are not throttling

- **`ClassPolicy`** (`5.7.x` about our IP): temporary for the address (never `invalid`), but **not
  counted as throttling** — slowing down does not grow a PTR record; if it counted, one blocked IP
  would calibrate every provider to zero (invariant 6).
- **Per-recipient deferrals** (greylisting, `4.2.2` over-quota): retried, but must **not** drive the
  shared pacer down or arm the MX pause. This is a fix `ds-smtp-retry` already carries — only
  `IsThrottle()` (`421`/timeout/reset) moves the pacer; `IsTemp()` only schedules a retry. See
  `aimd-pacing.md`.

## When a server has decided about us (plan 007)

`ClassPolicy` is about the connecting client and holds for the whole session. After
`probe.policy_stop` **consecutive** policy replies the session ends and the rest of the batch is
reported `connected:false` with `not attempted:` — continuing would spend a token per recipient on an
answer already known, and keep hammering a server that has just said no, which is how a soft block
hardens.

Consecutive matters. A single `5.7.x` can be a per-recipient policy — a distribution list that
rejects external senders — and stopping a fifty-recipient batch on one reply would throw away
forty-nine answers. The counter resets on any non-policy reply.

None of this reaches the pacer (invariant 6). Remembering the refusal *across* requests is plan 010's
job, where it belongs with IP health and the alert.

## Catch-all versus randomiser (plan 005)

A `250` is only worth something if the server would have said `550` to a name that does not exist.
Establishing that takes several known-bad local parts, not one:

| Bogus probes | Meaning | Scope |
|---|---|---|
| all accepted | **catch-all** — the domain takes anything | the domain |
| all rejected | the server answers honestly; the real replies stand | — |
| anything between | **randomiser** — the server answers by coin flip | **the server** |

One probe cannot tell the last case from the first two: it lands on accept or reject more or less at
random, so the same domain reports catch-all on one run and clean on the next, and a real mailbox
behind it is reported `valid` on the strength of a `250` that meant nothing.

The scope column is the part that is easy to get backwards. A catch-all is one domain's business. A
randomiser is the host's, so it condemns every domain behind it — which is why the verdict is
remembered per MX host (`mx:<host>:randomiser`) rather than per domain, and why Data Scout's own
per-domain catch-all cache does not cover it.

## Data Scout status reconciliation (plan 005)

**Superseded by ADR-006.** This service returns facts — `accepted`, `catch_all`, `randomiser`, the
SMTP code and its enhanced code — and Data Scout scores them into
`email_verifications.status` with the machinery it already has. There is no reconciliation table
here to keep in sync.

## Testing

Table-driven over `(code, text) → Class`. The SMTP transport is behind an interface so no test opens
a socket.
