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
  would calibrate every provider to zero (invariant 5).
- **Per-recipient deferrals** (greylisting, `4.2.2` over-quota): retried, but must **not** drive the
  shared pacer down or arm the MX pause. This is a fix `ds-smtp-retry` already carries — only
  `IsThrottle()` (`421`/timeout/reset) moves the pacer; `IsTemp()` only schedules a retry. See
  `aimd-pacing.md`.

## Data Scout status reconciliation (plan 005)

Data Scout's `email_verifications.status` vocabulary (`verified/valid/accept_all/risky/invalid/
unknown`) is the contract the HTTP response must map onto. The reconciliation table lives in plan
005; this classifier produces the four internal verdicts and the boundary maps them.

## Testing

Table-driven over `(code, text) → Class`. The SMTP transport is behind an interface so no test opens
a socket.
