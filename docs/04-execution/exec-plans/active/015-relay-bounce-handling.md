# Plan 015 — relay-bounce-handling

**Status:** Active — **rewritten 2026-09-11**; the return path is decided and the ingest is built
**Phase:** C
**Depends on:** 014, **020** (010 and 011 are Complete but dark)
**Blocks 014's first production message:** 014 sends *from* the address this plan must be able to
receive at.

## Goal

Close the loop on sending: capture bounces and complaints, feed them into IP health, and suppress
hard-bounced addresses — so the isolated IP stays clean and a dead address is never mailed twice.

## Rewritten 2026-09-11 — the part the original plan assumed away

The original said "a return-path/VERP mailbox or webhook the relay controls" in one clause, as
though such a thing existed. It does not, and **there is no obvious way to create one**:

- **The node must not receive mail.** `operations/deployment.md` says nothing may listen on `:25`,
  and it is right: a stray listener there is an open-relay risk that burns the IP faster than any
  probing mistake. So bounces cannot simply be delivered to the box that sent the message.
- **`datascoutmail.com` is routed by Cloudflare Email Routing**, which *forwards* to another
  mailbox. There is no store the service can read, and a forwarded bounce loses the envelope that
  identifies which message it belongs to.

So this plan's first task is a decision nobody has taken, and it must be taken **before 014 sends
its first production message** — because a bounce for an already-sent message arrives at the
envelope sender whether or not anyone is listening, and changing the sender afterwards leaves a
window of mail whose bounces are lost.

## The return path — **decided 2026-09-11: Cloudflare Email Worker → `POST /bounce`**

| | How it works | Cost |
|---|---|---|
| **A. Cloudflare Email Routing → Worker → webhook** | `bounces.datascoutmail.com` MX at Cloudflare; an Email Worker posts the raw message to `POST /bounce` on this service | No new mail infrastructure, reuses the mTLS edge. Needs a Worker, and Cloudflare's inbound size limits apply |
| **B. An IMAP mailbox the service polls** | `bounces.` MX at any mailbox provider; the relay polls and parses | Conceptually simplest, but adds a credential, a poll loop, and a second place mail lives |
| **C. An inbound MTA on a *different* host** | A small receiver elsewhere, forwarding parsed DSNs | Most control, most to run, and a second machine to keep clean |

**Chosen: A.** Cloudflare already holds the zone and already routes the domain's mail, the
Worker is a few lines, and the delivery lands on the authenticated HTTP edge this service already
has. **B** is the fallback if Worker size limits prove awkward for large DSNs. **C** is only worth
it if bounce volume ever justifies its own host — which, for transactional mail from one product, it
will not.

Whichever is chosen, the envelope sender becomes VERP — `bounces+<token>@bounces.datascoutmail.com`
— so a bounce identifies its message without parsing the body, which is the part of DSN handling
that is unreliable across servers.

## Design

- **Ingest**: `POST /bounce` (authenticated like every other non-health route), the raw message.
  Parse the DSN status: `5.x.x` hard, `4.x.x` soft, and feedback-loop complaints separately.
- **Hard bounce (`5.1.x`)** → suppress locally *and* push the signal to Data Scout, which owns the
  list (011). Never mail it again.
- **Soft bounce** → bounded retry through 014's queue, then give up. A soft bounce is the sending
  twin of a greylist: the server said "later", and "later" is a schedule, not a verdict.
- **Complaint** → suppress, and weight IP health down. A rising complaint rate pauses *sending*
  while leaving verification alone: they share an IP but a complaint is about mail, and stopping
  probing would not improve it.
- **A bounce is not a verification verdict.** A hard bounce for an address this service once called
  `valid` is new evidence about the address, and it belongs in Data Scout's verdict table through
  the suppression signal — not written back into a probe result here, which holds no business data
  by design (ARCHITECTURE).
- Metrics into 009: `relay_bounces_total{class}` and a complaint rate, both bounded by construction
  — **no per-address or per-domain label**, for the reason `metrics.md` already records about MX
  labels.

## Tasks

- [x] **Return path decided** — Cloudflare Email Worker, ingest at **Data Scout** (2026-09-11)
- [x] MX and SPF for `bounces.datascoutmail.com` — done 2026-09-11
- [x] The Worker: `bounces+…` to `POST /bounce`, **everything else forwarded**, forward on error too
- [x] Zone catch-all repointed at the Worker, after its code was deployed
- [ ] VERP envelope sender with a token that identifies the message
- [x] **Data Scout:** `POST /bounce` + DSN parsing; complaints distinguished from bounces —
      **live and proven in production 2026-09-11**, see Results
- [x] **Data Scout:** hard bounce → **a verification verdict, not the suppression list** — see the
      correction below; "never mail it again" is enforced in the outbox, where the record is
- [x] **Here:** `POST /admin/ip-health/complaint`; a spike pauses sending and leaves verification running
- [ ] Soft bounce → bounded retry via 014's queue; complaints → suppress + IP-health penalty
- [x] A complaint spike pauses sending and **not** verification — tested
- [x] Metrics: `relay_complaints_total` here; the hard-bounce share is read from Data Scout's own
      records by its `healthcheck.sh`, which is where that data lives and how that box alerts
- [ ] Data Scout: accept the suppression signal; a hard bounce updates the address's record
- [x] Tests: a hard bounce records a verdict; a complaint does not; a soft bounce writes nothing;
      `not-spam` is not a complaint; a malformed DSN produces no verdict at all
- [ ] Update `service/mail-relay.md`, `operations/ip-reputation.md`, `metrics.md`, changelog in both
      repositories

### Correction — a hard bounce is a verdict, not a suppression (2026-09-11)

This plan said to suppress a hard-bounced address. Suppression in Data Scout is the do-not-reingest
list, and a suppressed address makes verification return a transient `unknown` and store nothing —
so suppressing on a bounce would mean a customer asking about a dead address gets "we cannot say"
instead of `invalid`. That withholds exactly the answer the bounce just proved, and the one they are
paying for.

A bounce is the strongest evidence about an address there is, stronger than any probe, because the
message was delivered somewhere and refused. It is recorded as the verdict. "Never mail it again"
then belongs in the outbox, where the record already is — and the relay needs no second list.

## Results (2026-09-11) — the path is live end to end

Both sides deployed and exercised against production, not a fixture.

| check | result |
|---|---|
| `POST /bounce` with no token | `401` |
| with a wrong token | `401` |
| with the right token, unreadable report | `204` — accepted and logged, because the Worker's only alternative is mail in a human inbox |
| with a **real DSN** | `204`, and the row appeared: `invalid / bounce / score 0 / 5.1.1 / bounced=true / "smtp; 550 5.1.1 User unknown"` |
| verifier `POST /admin/ip-health/complaint` | `200`, and `relay_complaints_total 1` in the scrape |

The test address was removed afterwards; nothing of it is left in the verdict table.

**And the risk the catch-all switch actually carried was checked**: mail to `postmaster@` still
arrives. That address is where DMARC `rua` reports land, and it now passes through a script that
sits in front of the whole domain's mail — which is why the Worker forwards by default and forwards
on every internal error. Confirmed working after the switch.

**What this does not yet prove** is the half that needs a real message: a bounce produced by
actually sending to a dead address, arriving through Cloudflare's Worker rather than through `curl`.
That is the manual gate, and it waits on the warm-up ladder like 014's does.

## Definition of Done

- [ ] A real bounce, produced by sending to a known-dead address, arrives and is parsed — recorded
      here with the DSN
- [ ] It marks the address and updates suppression on **both** sides
- [ ] A complaint penalises IP health; a spike pauses sending while verification continues — tested
- [ ] An unparseable DSN never produces a verdict
- [ ] `relay_bounces_total` has bounded cardinality
- [ ] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [ ] Docs updated per `CLAUDE.md` Phase 5; changelog entries in both repositories
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Notes / decisions / deviations

Suppression's source of truth stays Data Scout (011): this plan pushes signals to it and keeps a
local copy for immediate enforcement. The local copy is digests, never addresses — a bounce handler
that accumulated a list of real addresses on this host would rebuild the liability plan 011 spent
its design avoiding.

**The dependency arrow between 014 and 015 points both ways**, and pretending otherwise is what made
the original pair unbuildable in order. 015 needs 014 to send; 014 needs 015's decision to know what
address to send *from*. The resolution: **decide the return path first, build 014, then build 015** —
the decision is cheap, the sender is impossible to change cleanly afterwards.

**DMARC stays at `p=none` until this plan is running** (decided 2026-09-11). Tightening to
`quarantine` before there is a bounce and complaint feed is how legitimate mail disappears silently:
the policy would start rejecting on alignment failures nobody can see yet. The trigger to move is
this plan's own signal — once `relay_bounces_total` and the complaint rate exist and have been quiet
through a warm-up, `p=quarantine`, then `p=reject`.

This is the last plan of Phase C. After it the IP is a self-policing sender: it stops itself when
listed, stops itself when complained about, and never mails an address twice that has already said
no.
