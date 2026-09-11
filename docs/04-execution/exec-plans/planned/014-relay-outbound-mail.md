# Plan 014 — relay-outbound-mail

**Status:** Planned — **rewritten 2026-09-11** against what the two repositories actually are now
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

### The interface — decision required

**Option A: `POST /send` (the original design).** Data Scout gains a third provider beside
`postmark` and `smtp`, its outbox drains into it. The boundary, mTLS, the API key and the firewall
rule are already built and proven for HTTP; nothing new is exposed.

**Option B: SMTP submission on `:587`, authenticated.** Data Scout changes `smtp_host` and nothing
else — its `smtp` provider already works and is deployed. Cheapest possible integration.

**Recommended: A.** B looks cheaper and is not: it means an inbound listener on a host whose
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
on content, complaint rate and volume shape, and needs its own ramp. DMARC is `p=none`, which is
right for a sender nobody has seen yet and wrong to leave there. **This plan may be built while the
probe ladder runs; production mail must not be switched over until it finishes** — and then with its
own ramp, not on day one.

## Tasks

- [ ] **Decide the interface** (A or B above) and the envelope sender with 015
- [ ] `internal/relay`: message assembly + DKIM signing with `s1`
- [ ] A durable queue (Redis, AOF) — accepted means it survives a restart
- [ ] `POST /send` (authenticated) + draining through the existing pacer and central bucket
- [ ] Suppression and IP health checked before each send, **both failing closed**, including a stale
      list
- [ ] Transient-failure retry with backoff
- [ ] Data Scout: a provider beside `postmark`/`smtp`, wired to the existing outbox drain
- [ ] Tests: a signed message assembles correctly; suppressed → never sent; unhealthy IP → never
      sent; stale suppression → never sent; **the prober still never sends `DATA`**
- [ ] A restart mid-queue loses nothing — tested
- [ ] Update `api.md`, `service/mail-relay.md` (an 8-line stub today), `features/006-mail-relay.md`,
      `SECURITY.md`, changelog in both repositories

## Definition of Done

- [ ] A signed test message is delivered and passes SPF + DKIM + DMARC at mail-tester — recorded
      here with the score
- [ ] Verification and send are separate code paths; **verify still never sends `DATA`** — asserted
- [ ] A suppressed address is never sent to; an unhealthy IP stops sending; a stale suppression copy
      stops sending — each tested
- [ ] A message accepted by `POST /send` survives a service restart and is delivered after it
- [ ] Sending and verification share one pacer: a busy MX slows both — shown
- [ ] The envelope sender is one 015 can receive bounces at, and that has been demonstrated before
      the first production message
- [ ] `go test -race -count=1 ./...` green; `go vet`, `gofmt -l .`, `golangci-lint run` clean
- [ ] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [ ] Docs updated per `CLAUDE.md` Phase 5; changelog entries in both repositories
- [ ] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated
- [ ] **If the relay ships disabled, this DoD says so and names who turns it on** — the gap that let
      010 and 011 sit dark for a fortnight (see 020)

## Notes / decisions / deviations

Sending reuses the verification IP's warmed reputation; sequencing verify first is what makes this IP
a credible sender at all. Keep verify and send strictly separated in code — they share the pacer and
the limiter because the receiving server sees one IP, and nothing else.

**Open, and owned by 015:** the return path. Until it exists this plan can be built and tested but
must not send production mail, because mail sent from an address whose bounces nobody reads is how a
warmed IP is spent.
