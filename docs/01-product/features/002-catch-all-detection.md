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

Microsoft-class hosts answer inconsistently. With a single bogus probe the coin lands on accept or
reject, so the same domain reports catch-all on one run and clean on the next — and a real mailbox
behind it gets reported valid on the strength of a `250` that meant nothing. `probe.catch_all_probes`
(default 3) is what turns "it said yes once" into "it says yes to everything" or "it says yes at
random".

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
