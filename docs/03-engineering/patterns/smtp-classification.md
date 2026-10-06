# Pattern — SMTP reply → verdict

The single source of truth for turning a reply into `valid | invalid | risky | unknown`. Ported from
`ds-smtp-retry/ratecheck/internal/prober`. Lives in one place (`internal/prober`); nowhere else maps
a code to a verdict.

## The core rule (invariant 1)

**A rejection of *us* is never a rejection of the address.** Only a `5xx` on `RCPT` *after* a good
`MAIL FROM` may mean "no such mailbox". Everything else that fails — connection refusal, TLS error,
timeout, 4xx, a blanket 550 before `MAIL FROM` — is `unknown`.

### Enforced in code, not only in the table (plan 027)

Two places make the rule structural rather than a property of the hint lists:

- **A reply code is three ASCII digits with a first digit of 2–5** (`replyCode`, RFC 5321 §4.2).
  `strconv.Atoi` took `999`, `600`, `-55` and `+25`; `"999 no such user"` used to reach
  `classifyPermanent` and come back `invalid`. Now such a line is a read error — a broken session,
  `conn_error` — and `Classify` itself returns `unknown` for any code outside 200–599, ahead of
  every other rule (the fuzzer found `621 greylist` slipping through as `deferred` when the range
  check sat after the greylist wording).
- **Nothing before `RCPT` can be a verdict** (`beforeRCPT`). A refusal at the banner, `EHLO` or
  `MAIL FROM` goes through `Classify` for its *kind* (throttled, deferred, policy …), but a result
  that would be `valid` or `invalid` becomes `policy` for a 5xx and `unknown` otherwise. Postfix's
  `reject_unlisted_sender` answers `MAIL FROM` with `550 5.1.0 <verify@…>: Sender address rejected:
  User unknown in virtual mailbox table` — subject 1 plus mailbox wording, which `Classify` reads as
  `invalid` — and before plan 027 that class was stamped on every address in the chunk. Data Scout
  derives `accepted` from `class`, so one refusal of our envelope sender recorded a whole batch
  undeliverable.

## Read the enhanced code first (RFC 3463)

The `5.X.Y` enhanced status answers "who is this about" before the prose. Subject `X`:

| Reply | Verdict | About |
|---|---|---|
| `250` | `valid` — **and meaningless unless `catch_all` is false** (invariant 7) | mailbox accepts |
| `550 5.1.1` NoSuchUser | invalid | recipient |
| `550 5.2.2` mailbox full | valid | mailbox exists |
| `452 4.2.2` over quota | unknown (retry) | recipient's box — **not our rate** |
| `550 Mailbox over quota` / `552 … mailbox full` / `… exceeded storage allocation` (no code, or 5.2.x) | valid | the same full mailbox said in words (plan 035) — it exists |
| `550 Quota exceeded: too many messages per hour …` / `5.7.x … sending quota` | policy | a quota on **our** sending — about us, never the address (invariant 1; plan 035) |
| `450`/`451` greylisting | unknown (retry) | per-recipient, rate-independent |
| `550 5.7.x` / `554 …blocked` / reverse DNS | **policy** → unknown | **our IP** |
| `421 4.7.x` / unusual rate | throttled → unknown (back off) | our rate |
| `550` naming a spam blocklist in prose, no enhanced code (Hetzner `rbl.your-server.de`) | **policy** → unknown | **our IP** |
| `550 A TLS connection is required`, no enhanced code (plan 024) | **policy** → unknown | **our session** — the prober has no STARTTLS step |

## Read the whole reply (plan 022)

A multi-line reply puts its reason first and its sign-off last. The reader keeps every line —
bounded, but always read to the final one — because classifying the last line alone turned Hetzner's
refusal of our IP into `550 Ihrem Serveranbieter erfahren.`, which no hint matches and which
therefore fell through to `invalid`. The enhanced code is still read from the start of the text, where
RFC 3463 puts it.

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

## A refusal that names us is about us (plan 030)

The wording lists can never be complete, so the classifier also checks for a fact it knows: **who we
are.** `ClassifyAs(code, text, Identity)` receives the node's source IP, HELO name and `MAIL FROM`
domain (`prober.Options.SourceIP`/`Helo`/`MailFrom`), and a permanent reply that names any of them **as a
whole token** counts as sender wording — `92.222.87.97` matches in `IP=92.222.87.97 -` and
`[92.222.87.97]>`, never in `192.222.87.971`. Precedence is unchanged: a recipient code (`5.1.x`, `5.2.x`)
or explicit mailbox wording still wins, so IONOS's "mailbox unavailable … `v=<our IP>`" (case r1601)
stays `invalid`. `Classify(code, text)` is the identity-less form the fuzzers and wording tests use.

**A temporary reply demanding encryption is policy, not a throttle.** `421 4.7.0 STARTTLS is mandatory`
used to class `throttled`, so `IsThrottle()` halved the pacer's rate for a host that refuses us at any
rate. Any `4xx` mentioning STARTTLS, `tls` as a word, or encryption is now `ClassPolicy` (never a
throttle, invariant 6); greylisting still wins over it.

**The corpus.** `internal/prober/testdata/corpus.tsv` holds every distinct permanent reply the warm-up
ladder received (394, from 710 rows, addresses redacted, our identity kept), with its golden class.
`TestCorpusClassifiesExactlyAsCommitted` fails on any change in what a real reply means; regenerate with
`go test ./internal/prober -run TestCorpus -update-corpus` and read the diff. At introduction 8 rows moved
`invalid → policy` and none the other way.

**The default stays `invalid`** (step 5). Of the corpus's no-code, no-hint `invalid`s (16 distinct, 33
rows) two were about us and gained wording ("you are listed on …", "Sender IP address rejected"); the
other 14 are recipient-shaped ("Address unknown", "Unknown recipient", "Unroutable address"). Turning
the default to `unknown` would cost those real verdicts to catch nothing the identity rule does not.

## Catch-all versus randomiser (plan 005)

A `250` is only worth something if the server would have said `550` to a name that does not exist.
Establishing that can take more than one known-bad local part — but since plan 029 only as many as
the answers need: none unless a real address was accepted, then one at a time, stopping at the
first rejection (a rejected first one is *clean*; an accepted one earns the next, up to
`probe.catch_all_probes`), plus a 5% audit sample of the full three:

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
a socket. The session is one table (`session_table_test.go`) whose every row also asserts "no
`invalid` without an `RCPT` answer", and `FuzzClassify` holds invariant 1 as a property over every
integer code (plan 027; target list in `testing/strategy.md`).
