# Plan 014 — relay-outbound-mail

**Status:** Active — **rewritten 2026-09-11** against what the two repositories actually are now.
**Code complete and shipped disabled**; as of 2026-09-14 every Definition-of-Done item that can be
proven without sending a real message is ticked with its evidence, and the switch-on condition and
order are written into the DoD's last item. What remains is the live gate: a signed message at
mail-tester and the bounce it provokes — both waiting on the warm-up ladder and on the Abusix
listing from ladder day 4 expiring (~2026-09-17)
**Phase:** C
**Depends on:** 003, 013, **020** (010 and 011 are Complete but dark — 020 is what makes them real)

## Goal

Send transactional mail from the isolated, warmed IP — DKIM-signed, paced per recipient MX,
honouring suppression and IP health — so the main product IP never sends outbound mail either.

## Rewritten 2026-09-11 — read this first

The original plan was written before three facts existed. None of them change the goal; all three
change the work.

1. **Data Scout already sends mail, and already has the seam.** It has a durable `email_outbox` with
   a drain and a provider abstraction (`postmark` | `smtp` | unset). Production today:

   ```
   provider: smtp   host: smtp.gmail.com   from: arapan.gabriel.v2@gmail.com
   ```

   Password resets leave from a personal Gmail address. That is the thing this plan replaces, and it
   is a stronger motivation than the original text had.

2. **The protections this plan checks before every send are switched off.** `ip_health.resolvers` is
   empty and `suppress.enabled` is `false`. Plan **020** turns them on and is now a hard dependency:
   building the relay's safety checks against two disabled switches would produce decoration.

3. **There is no return path**, which is plan 015's problem but it constrains this plan's envelope
   sender. See *The envelope sender and VERP* below.

## Context

Phase 2 of the scope (ADR-002). Everything Phase A/B built is reused: the per-MX pacer, the central
bucket, IP health, suppression. What is new is message assembly, signing, a queue that survives a
restart, and a code path that sends `DATA` — the one thing verification must never do (invariant 8).

## Design

### The interface — **decided 2026-09-11: `POST /send`**

**Option A: `POST /send` (the original design).** Data Scout gains a third provider beside
`postmark` and `smtp`, its outbox drains into it. The boundary, mTLS, the API key and the firewall
rule are already built and proven for HTTP; nothing new is exposed.

**Option B: SMTP submission on `:587`, authenticated.** Data Scout changes `smtp_host` and nothing
else — its `smtp` provider already works and is deployed. Cheapest possible integration.

**Chosen: A.** B looks cheaper and is not: it means an inbound listener on a host whose
operations doc says in as many words that nothing may listen for mail, and it would duplicate the
authentication, the firewall rule and the mTLS boundary that already exist for HTTP. The rule "this
host accepts no mail" is worth more as a rule with no exceptions than the days B would save. A costs
one provider class on their side, beside two that already exist.

Recorded as a decision because it changes both repositories.

### The envelope sender and VERP

013's identity table gives relay `noreply@datascoutmail.com` with HELO `mail.datascoutmail.com`.
That is fine for sending and **not sufficient for receiving bounces**, which plan 015 needs: a
bounce goes to the envelope sender, and `datascoutmail.com` is routed by Cloudflare Email Routing,
which forwards mail rather than giving anyone a mailbox to read.

So the envelope sender must be decided **here**, before the first message leaves, because changing
it later means a period during which bounces for already-sent mail arrive somewhere nobody reads.
VERP — `bounces+<token>@bounces.datascoutmail.com` — requires that sub-domain to have an MX pointing
at something 015 can consume. **015 owns that choice; this plan must not send until it is made.**

That ordering is the reverse of the original plans' dependency arrow, and it is deliberate: 015
depends on 014 for the sending half, 014 depends on 015 for the address it sends *from*.

### The rest

- `internal/relay` — assemble, **DKIM-sign** with `s1` (the key is on the node and the selector is
  published and verified), enqueue.
- The queue must **survive a restart**: a message accepted and then lost is worse than one refused.
  Redis with AOF is already on the node for exactly this class of state (plan 006 chose `everysec`).
- Draining goes through the **same per-MX pacer and central bucket** as verification. One pacing
  model for the IP, because the receiving server does not care which of our two activities it is.
- **Suppression and IP health are checked before each send, and both fail closed.** This is the
  difference from the verify path, which is loud but not fatal: verification has an authoritative
  upstream check, and sending has nothing between the queue and the socket. A stale suppression copy
  stops sending.
- **A separate code path from verification.** Invariant 8 is not a convention to be careful about;
  the relay and the prober share the pacer and the limiter and nothing else, and a test asserts the
  prober still never writes `DATA`.
- Transient failures retry with backoff. Unlike plan 006's deliberate refusal to retry
  verification — where a retry's answer had nowhere to go — a send retry has an obvious owner: the
  queue is ours.

### Sending reputation is not probing reputation

The warm-up ladder now running warms *connection and recipient-probing* behaviour. Sending is judged
on content, complaint rate and volume shape, and needs its own ramp. **This plan may be built while
the probe ladder runs; production mail must not be switched over until it finishes** — and then with
its own ramp, not on day one.

**DMARC on `datascoutmail.com` is `p=reject; sp=reject` since 2026-09-17, and that is now a
precondition on this plan rather than a note.** It was moved off `p=none` because the domain was
being forged — NTT Docomo's report for 2026-09-16 carried three messages with `From:
datascoutmail.com` from consumer addresses in India, Costa Rica and Argentina, all SPF hardfail and
unsigned, all delivered because the policy said to deliver them. The domain sends no message mail
today, so `reject` costs nothing and stops the forgeries landing (Data Scout's plan `086`).

**The consequence for the first real send from here:** it must be DKIM-signed *before* it goes out,
not after a bounce report explains why it did not arrive. Being inside the SPF record is not enough —
alignment is what DMARC evaluates, and an unsigned message whose envelope domain differs from the
`From:` fails it under `reject`. The signing task below is therefore a gate on the first send, and
`s1`'s public key must be published and resolvable before the relay is switched on.

## Tasks

- [x] **Interface decided** — `POST /send` (2026-09-11). The envelope sender follows 015's return
      path, decided the same day: VERP at `bounces.datascoutmail.com`
- [x] `internal/relay`: message assembly + DKIM signing with `s1`
- [x] A durable queue (Redis, AOF) — accepted means it survives a restart
- [x] `POST /send` (authenticated) + draining through the existing pacer and central bucket
- [x] Suppression and IP health checked before each send, **both failing closed**, including a
      stale list — and suppression re-checked at send time, not trusted from accept
- [x] Transient-failure retry with backoff
- [x] Data Scout: a provider beside `postmark`/`smtp`, wired to the existing outbox drain
- [x] Tests: the RFC's own canonicalization example; a signature verified the way a receiver would
      check it; suppressed / unhealthy / stale all refuse; dot-stuffing; STARTTLS attempted when
      offered; permanent and transient told apart
- [x] A restart mid-queue loses nothing — tested
- [x] Update `api.md`, `service/mail-relay.md`, `features/006-mail-relay.md`, `SECURITY.md`,
      changelog in both repositories

## Definition of Done

- [ ] A signed test message is delivered and passes SPF + DKIM + DMARC at mail-tester — recorded
      here with the score
- [x] Verification and send are separate code paths; **verify still never sends `DATA`** —
      asserted 2026-09-14 against the wire, not the code: `TestVerificationNeverSendsDATA` records
      every command a probe puts on the socket (including the catch-all probes) and fails on
      `DATA`/`BDAT`. It was the one invariant the whole two-service split rests on that nothing
      checked; a refactor now fails here instead of at a receiving server
- [x] A suppressed address is never sent to; an unhealthy IP stops sending; a stale suppression
      copy stops sending — each tested: `SuppressionFailsClosedForSending`,
      `ABurnedIPStopsSendingEntirely`, `SuppressionIsRecheckedAtSendTime` (accept-time agreement is
      not trusted at send time), plus `NoBudgetDefersRatherThanSends`
- [x] A message accepted by `POST /send` survives a service restart — `AQueuedMessageSurvivesTheProcess`
      (the queue is read back from Redis by a second instance). *Delivered* after it is part of the
      live gate below, since nothing has sent a real message yet
- [~] Sending and verification share one pacer — **one `pace` instance is passed to both**
      `prober.New` and the relay in `cmd/verifierd/main.go`, and `DeliveryOutcomesDriveTheQueueAndThePacer`
      shows the relay's outcomes feeding it. A cross-leg demonstration (a busy MX visibly slowing
      the *other* leg) needs both legs live and belongs with the mail-tester gate
- [x] The envelope sender is one 015 can receive bounces at — `bounces+<id>@bounces.datascoutmail.com`,
      routed at Cloudflare to an Email Worker that posts to Data Scout's `POST /bounce` (live
      2026-09-11), and since 2026-09-14 the `<id>` resolves: Data Scout stores the `message_id`
      this service answers with and matches a recipient-less DSN through it. The remaining
      demonstration is a real bounce from a real send, which is the gate below
- [x] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean —
      re-run 2026-09-14, 0 issues
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed for the 2026-09-14 change
      (one test, one config comment, plan bookkeeping — no new surface, no new dependency)
- [~] Docs updated per `CLAUDE.md` Phase 5 — `service/mail-relay.md`, `features/006-mail-relay.md`
      and both changelogs are current as of 2026-09-14. `api.md`'s `POST /send` row and
      `SECURITY.md` were written with the feature and still describe it. The last doc edit this
      plan owes is the score from the mail-tester gate, which does not exist yet
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated
- [x] **The relay ships disabled, and here is who turns it on and when** — written down rather
      than left to be rediscovered, which is the gap that let 010 and 011 sit dark for a fortnight
      (see 020).

      **State:** `relay.enabled: false` in `config/verifierd.yaml` on the node, and Data Scout's
      `EMAIL_PROVIDER` is not `relay` — so both ends are off, and either one alone would do
      nothing.

      **Condition to turn it on** (all three, checked on the day, not assumed):
      1. The warm-up ladder has reached its sustained ceiling and held it — the ladder is in
         `docs/03-engineering/operations/ip-reputation.md`.
      2. `GET /admin/ip-health` reports the sending IP clean on every configured zone. As of
         2026-09-12 it was not: an Abusix `black` listing from the ladder's day 4, which expires
         ~2026-09-17 and is what plan 021 made visible.
      3. A test message to mail-tester scores well and passes SPF + DKIM + DMARC (the gate above),
         and its bounce — deliberately provoked to a dead address — arrives through the Worker.

      **Who:** the operator, in this order, because the reverse order sends mail nobody reads:
      `relay.enabled: true` + restart `verifierd` → confirm `POST /send` answers `202` → set
      `EMAIL_PROVIDER=relay` in Data Scout's `.env` through CD → send one real password reset to a
      mailbox you own and read the headers before letting customer mail follow.

## Notes / decisions / deviations

Sending reuses the verification IP's warmed reputation; sequencing verify first is what makes this IP
a credible sender at all. Keep verify and send strictly separated in code — they share the pacer and
the limiter because the receiving server sees one IP, and nothing else.

**Open, and owned by 015:** the return path. Until it exists this plan can be built and tested but
must not send production mail, because mail sent from an address whose bounces nobody reads is how a
warmed IP is spent.
