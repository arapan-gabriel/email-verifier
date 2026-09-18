# Plan 025 — the apex of `datascoutmail.com` explains the prober

**Date**: 2026-09-18
**Author**: Gabriel Arapan (+ Claude)
**Status**: In Progress — the page is written and reviewed against the code (`web/datascoutmail/index.html`).
Publishing it and repointing the two mailboxes are dashboard steps.

**Depends on**: Data Scout plan `086` (the product moved to `getdatascout.com` on 2026-09-17/18, so
this apex is free). **Unblocks**: `086`'s section G and its Definition of Done.

---

## Goal

A postmaster who sees `92.222.87.97` or `mail.datascoutmail.com` in a log and opens the domain must
find out what connected, who runs it and how to be excluded — in one screen, without a product
pitch. Until now that apex served Data Scout's marketing landing, which answered none of those
questions and tied the probing IP to the product's brand.

---

## Context

This domain belongs to the verifier alone: PTR, HELO (`config/verifierd.yaml`), the envelope sender
`verify@probe.datascoutmail.com` (plan `019`), the relay's return path (plans `014`/`015`) and the
mTLS certificate SAN all carry the name. Data Scout's ADR-014 made the split explicit and its
decision 5 states the reason this page exists: **separation is for reputation, not concealment** —
an operator who cannot be identified is treated worse by blocklist operators than one who publishes
what it does.

`datascoutmail.com` has been at `p=reject; sp=reject` since 2026-09-17, after consumer addresses
were caught forging the name at `p=none`.

---

## Design

A single static page, no JavaScript, no external requests, served from Cloudflare (the same hosting
the product placeholder used, which this replaces). It says, in order: this host verifies mailboxes
and never delivers mail; the exact envelope, HELO and PTR identifiers; what is kept and for how
long; that a rejection of *us* is recorded as `unknown`, never as "no such mailbox" (invariant 1);
how to be excluded; and why the name is separate from the product's.

**Every claim was checked against this repository before it was written down**, and two were wrong
in the first draft and corrected:

1. *"One session at a time per mail server"* — the pacer holds a per-MX rate with AIMD inside
   calibrated bands, not a single session. The page now says a rate that backs off and climbs.
2. *"The suppression list is consulted before any connection is opened"* — `internal/relay` checks
   it, the probe path does not (`internal/api/probe.go` has no suppression call; the relay's own
   comment says the authoritative check runs upstream). Data Scout checks it before verifying
   (`verify_tasks.py`, plan `040`), which is a different place with a different guarantee. The page
   now describes what actually happens: the domain and its known addresses go on the list Data Scout
   checks before an address is verified.

`DATA` is never issued — confirmed by grepping `internal/prober`, which is the claim on this page a
receiving postmaster is most entitled to hold us to.

---

## Tasks

- [x] `web/datascoutmail/index.html` — the page, claims checked against `internal/prober`,
  `internal/relay`, `config/verifierd.yaml` and Data Scout's `verify_tasks.py`
- [ ] Publish it on the `datascoutmail.com` apex (Cloudflare Pages), replacing the product landing
- [ ] Email Routing: `abuse@` and `postmaster@datascoutmail.com` → the `ops@getdatascout.com`
      mailbox (the catch-all Worker already forwards everything that is not a bounce there)
- [ ] `docs/08-decisions/changelog.md`
- [ ] Tell Data Scout's plan `086` that section G is done

---

## Definition of Done

- [ ] `https://datascoutmail.com/` serves the disclosure page: no pricing, no sign-up, the operator
      named, `abuse@` reachable
- [ ] A message to `abuse@datascoutmail.com` arrives in the `ops@` mailbox
- [ ] Nothing on the page contradicts the service — re-checked if the prober, the pacer or the
      suppression path changes
- [ ] Data Scout `086` section G ticked

---

## Notes / decisions / deviations

- **The page is in this repository, not Data Scout's.** The domain is the verifier's, and so is
  every fact on the page; keeping it here means a change to the prober and a change to what we
  publish about the prober land in the same review.
- **`robots` is `index, follow`** — deliberately findable. A postmaster searching the IP or the HELO
  name should reach it.
- **No contact form, no analytics, no fonts.** One file, no third-party requests: the page exists to
  be read from a log entry at 3 a.m., and anything it loads is another thing that can be blocked.
