# Plan 030 — A refusal of us is never a verdict

**Status:** Code complete 2026-10-02 — deploy and the manual-test gate pending
**Phase:** B
**Depends on:** 022 (whole reply), 024 (TLS prose, active), 027 (`beforeRCPT`) — and a Data Scout
companion change, below, without which half of the defect stays

## Goal

Close every path that turns a refusal of **us** into a statement about **the mailbox**, on both
sides of the wire: the verifier must never class it `invalid`, and Data Scout must never sell it as
`valid` (`deliverable`). Pin it with a table built from replies the warm-up ladder actually
received, so the next unseen wording is caught by structure rather than by adding one more phrase.

## Context

Invariant 1 has been fixed phrase by phrase: 022 (Hetzner's blocklist prose), 024 (TLS demands
with no code), 027 (anything before `RCPT` via `beforeRCPT`). Each fix was right and each was
narrow. Measured on 2026-10-01 by running the real replies through `Classify`
(`internal/prober/classify.go`) at the `RCPT` stage:

| reply (real, ladder 2026-09) | class today | about |
|---|---|---|
| `550 TLS encryption required for mails from 92.222.87.97` (day 16) | **invalid** | us |
| `550 Must use TLS` (day 17) | **invalid** | us |
| `550 Your IP 92.222.87.97 is not allowed to send mail here` (shape) | **invalid** | us |
| `554 IP=92.222.87.97 - Dialup/transient IP not allowed` (T-Online) | **invalid** at RCPT; `policy` today only because it arrives at the banner and `beforeRCPT` catches it | us |
| `550 Error: listed as abusive by MagicSpam` | **invalid** | us |
| `421 4.7.0 STARTTLS is mandatory` (`mx1.gesundheitswelt.de`) | **throttled** — and `IsThrottle()`, so it **halves the pacer's rate** | us |
| `551 5.7.1 <mail.datascoutmail.com[92.222.87.97]>: Client host rejected: Error Protokoll (DSGVO)` | policy ✓ | us |
| `550 Invalid RDNS entry for 92.222.87.97`, `550 Must issue STARTTLS`, `5.7.0 STARTTLS is mandatory`, `5.7.1 Session encryption is required`, Sophos `5.7.4 … TLS version is not available`, `5.4.4 Unable to relay`, `Relay access denied` | policy ✓ | us |

Why the misses: `classifyPermanent` falls back to **`ClassInvalid`** whenever a 5xx carries no
enhanced code and no hint matches (its last line), and the hints are prose lists. "TLS encryption
required" misses "tls required" and "encryption is required" by one word each. Every one of the
misses above **names our own IP**, which the classifier never looks for; the config knows it
(`probe.source_ip`), as it knows our HELO name and `MAIL FROM` domain.

**The other half is in Data Scout, and it is larger.** The verifier returned the namemaster row as
`class: policy`, `accepted: null` — correct. Data Scout's mapping (`app/core/verify/smtp_probe.py
_result_of`) turns that into `blocked=True`; the engine records `smtp_check=None`; and
`app/core/verify/scoring.py status_of` falls through to its last line, **`VALID` — "everything
checkable passed; mailbox unconfirmed"**, which `result_of` publishes as **`deliverable`** and
which is billable (`CONCLUSIVE`). Measured over ladder days 14-21: **every one of the 362 rows
stored `valid` was a non-answer** — 168 deferred (greylisting), 144 policy (93 of them EOP
`5.4.1`, plus STARTTLS demands and `Invalid RDNS entry for 92.222.87.97`), 50 throttled. Not one
`valid` row came from a mailbox being asked and answering. A customer reading `deliverable` is
being told the opposite of what happened. Separately, a session refused before `RCPT` (T-Online)
reaches Data Scout as `connected=false` and the engine's not-connected branch drops `smtp_class`
and the reply entirely (Data Scout tech-debt, 2026-10-01).

## Design

### Verifier

1. **Our identity in a reply is a sender hint.** `classifyPermanent` receives the node's identity —
   source IP, HELO name, `MAIL FROM` domain — and a 5xx whose text contains any of them counts as
   `sender`, with the existing precedence: an explicit mailbox hint still wins. That keeps IONOS's
   `550 mailbox unavailable … case?c=r1601&i=ip&v=92.222.87.97` `invalid` (it names us in a case
   link *and* says the mailbox is unavailable), and turns every miss in the table above into
   `policy`. Matching is on token boundaries (`92.222.87.97` must not match `192.222.87.971`).
   `Classify(code, text)` stays the single mapping point; it gains an identity argument through an
   `Identity` value on `Options`, not a package global.
2. **TLS prose that 024 missed:** `tls encryption`, `must use tls`, `encryption required`. 024's
   list stays; this extends it with the two measured misses. (031 removes most of the cause; this
   stays for servers whose TLS we still cannot meet.)
3. **Blocklist prose:** `listed as abusive`, `not allowed to send`, `dialup`, `dynamic ip`,
   `transient ip` — each one a measured or published refusal-of-client wording; `listed` alone is
   not added (it matches `unlisted`).
4. **A `421` that demands TLS is not throttling.** `case code == 421: return ClassThrottled` is
   checked first today; a 421 whose text matches the TLS hints becomes `ClassPolicy` (not a rate
   signal — invariant 6 — and `IsTemp`, so it is retried rather than decided).
5. **The default for an unrecognised 5xx is a measured decision, not a change made blind.** Today
   no code + no hints = `invalid`. Before touching it, this plan exports every `invalid` row of
   the ladder (452 of 29,573 rows in `warming/emails/day-*/*.result.json`) and counts
   those with no enhanced code and no mailbox hint. If they are a handful and all genuinely about
   mailboxes (`550 no such user here` shapes), the default stays and the corpus becomes test data;
   if any are about us, the default becomes `unknown`-shaped `policy` and the cost (fewer
   `invalid`s) is recorded. The number decides, in this plan's notes.
6. **The table is the test.** `classify_corpus_test.go` with every reply above verbatim (recipients
   redacted) and the class it must have, plus the export from step 5. A new miss in the field is a
   new row, not a new paragraph.

### Data Scout companion (its repo, its own plan number — listed here so neither half ships alone)

**Numbered 2026-10-01:** the status mapping is Data Scout plan `112` (`112-be-a-non-answer-is-not-deliverable`, which also covers `risky` rows of the same shape — 109 in production); the not-connected branch is Data Scout plan `106`.

- `scoring.status_of`: `block=True` (the server answered *us*, not the mailbox) → **`UNKNOWN`**, not
  `VALID`. `unknown` is outside `CONCLUSIVE`, so these rows also stop being billed. A deferred row
  keeps its `retry_after_seconds` and is rechecked as today.
- `VALID` keeps its meaning for the case it was written for — an SMTP leg that is switched off
  (tier off), where "everything checkable passed" is literally true.
- `engine.py` not-connected branch carries `smtp_class`, `smtp_code`, `reply` into `signals`, so a
  refusal before `RCPT` is visible to `run-day.sh`'s rule 1 and to support.
- Cost, stated: on the ladder's mix 362 of 18,959 rows (days 14-21, **1.9%**) move from
  `valid`/billable to `unknown`/free.

## Tasks

- [x] Export the ladder's `invalid` and `policy` replies (redacted) → `internal/prober/testdata/corpus.tsv` — 394 distinct from 710 rows (days 1-22), addresses redacted, our identity kept
- [x] Count the no-code, no-hint `invalid`s; record the number and the decision on the default (step 5) — 16 distinct / 33 rows; 2 were about us and gained wording; **default kept `invalid`** (the other 14 are recipient-shaped)
- [x] `Identity` on `Options` (source IP, HELO, MAIL FROM domain); `classifyPermanent` treats a token-bounded match as a sender hint — `Options.SourceIP` + `Helo` + the `MailFrom` domain; `ClassifyAs`; a recipient code (5.1/5.2) or mailbox wording still wins
- [x] TLS and blocklist prose (steps 2-3); 421 + TLS wording → policy (step 4) — any 4xx demanding STARTTLS/TLS/encryption → policy; greylisting still first
- [x] Tests alongside: the table above verbatim; IONOS r1601 stays `invalid`; `5.4.1` stays `policy`;
      `192.222.87.971` does not match our IP; a 421 TLS demand never reaches `Observe(throttled=true)`;
      the corpus file classifies exactly as committed — `internal/prober/identity_test.go`; mutation: identity off → 5 tests fail, the 4xx-TLS rule off → 2 fail
- [x] `docs/03-engineering/patterns/smtp-classification.md` — identity rule, 421 rule, the corpus — new section
- [x] Data Scout companion plan written and linked (status mapping + not-connected branch) — status mapping = Data Scout `112` (shipped 2026-10-01); not-connected branch = Data Scout `106`

## Definition of Done

- [ ] **Manual-test gate:** every reply in the Context table, sent through `POST /probe` against
      an mxsim profile scripted to answer it at `RCPT`, comes back with `accepted: null`; and the
      next ladder/customer day carrying any of these wordings records no `invalid` for them in
      Data Scout (query: `status='invalid' AND signals->>'smtp_reply' ~ '92\.222\.87\.97|tls'`).
      Recorded here
- [ ] Data Scout companion deployed: `SELECT count(*) … status='valid' AND (signals->>'block')::bool`
      is zero for rows written after it
- [x] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean; coverage gate ok — 2026-10-02, with Redis
- [x] `pr-checklist.md` — "us ≠ address" is this plan; confirmed against the corpus — us ≠ address is this plan, confirmed against the corpus; fail-closed and SSRF untouched
- [ ] Docs per Phase 5; `changelog.md`; Status Complete, moved to `completed/`, `ROADMAP.md`

## Notes / decisions / deviations

- **Why identity and not more phrases.** Every phrase list here has been one wording short when it
  mattered (022, 024, this plan's day-16 row). A server that names our IP in a 5xx is talking about
  us in every language; the IP is the one token no mailbox rejection needs.
- **Scope against 024.** 024 (active, code shipped) added TLS prose and is waiting on its
  deploy-and-observe box; this plan does not reopen it — it adds the two wordings it missed and
  the structural rule that would have caught both.
- **Not here:** STARTTLS itself (031), standing a family down on these refusals (032).
