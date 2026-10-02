# Feature 002 — catch-all and randomiser detection

**Status:** live (plans 001 and 005) · `docs/04-execution/ROADMAP.md` for the delivering plans.

## What it answers

A `250` on `RCPT TO` only means something if the server would have said `550` to a name that does
not exist. This feature establishes whether that is true, by asking about local parts that cannot
exist.

| Bogus probes | Verdict | Scope | Consequence |
|---|---|---|---|
| all accepted | catch-all | the domain | no `250` from this domain is evidence |
| all rejected | clean | — | the real replies can be trusted |
| anything between | randomiser | **the server** | no `250` from this host is evidence, for any domain it serves |

## Why one probe is not enough

Some hosts answer inconsistently. With a single *accepted* bogus probe the coin may have landed on
accept, so the same domain could report catch-all on one run and clean on the next — and a real
mailbox behind it be reported valid on a `250` that meant nothing. So an accepted bogus probe earns a
second (`probe.catch_all_probes`, default 2): accepted again → catch-all, rejected → randomiser.

**Since plan 029 a rejected first probe settles it.** A rejection cannot come from a catch-all, and
the warm-up's record showed how rare the coin flip is: over 6,987 answered rows (Data Scout days
19-21) one randomiser, on one host — and **not** Microsoft, whose 1,519 domains refused every bogus
probe. Asking two more after a rejection only sent questions to mailboxes that do not exist. The cost
is bounded and measured: a randomiser that rejects first is recorded clean, and a 5% audit sample
(`probe.catch_all_audit_rate`) asks the full three to keep that rate visible in
`verify_catch_all_probes_total`. **And nothing is asked unless a real address was accepted** — the
verdict qualifies a `250` and nothing else.

## Why the scope matters

Catch-all is one domain's business and is never projected onto its neighbours. A randomiser is the
*host's* business: the verdict is remembered against the MX host, so the next request for a
different domain on that server carries it without re-probing. Getting these two the wrong way round
is how a nonexistent mailbox gets reported as valid.

## Boundary

This service reports the facts (`catch_all`, `randomiser`); Data Scout scores them. It keeps its own
per-domain catch-all cache and decides `need_catch_all` before calling — the per-*server* randomiser
verdict is the part it does not track, which is why this service keeps it (ADR-006,
`docs/06-generated/redis-contract.md`).
