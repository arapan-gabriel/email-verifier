# Plan 008 — data-scout-integration

**Status:** Planned
**Phase:** A
**Depends on:** 001, 002, 003, 004, 005, 006, 007, **013** — there is nothing deployed to point
Data Scout at until 013 lands (verified on the node 2026-09-04: no `verifierd` unit, no
`/etc/verifierd/verifierd.yaml`, no binary, nothing listening but `:22`).

## Goal

Cut Data Scout over to this service: `smtp_probe.probe_many` becomes an HTTP client to `POST /probe`
and port-25 traffic leaves the production host for good. Per ADR-006 the seam is the prober, not the
provider — everything above it (layers 0–5, domain grouping, scoring, signals, the domain-profile
cache, quota, suppression, the verdict table) is untouched.

> **Paired with Data Scout's plan `073`, rewritten 2026-08-28.** That plan is the other half of this
> one and carries the detail on its side: the config, the response mapping, the `ENGINE_VERSION`
> bump, the mTLS material, the warm-up ladder and the manual test against production. Its
> **ADR-009** records why the probe left that repository at all; it supersedes the SOCKS5-proxy
> approach `073` originally described. Read both before starting either.

## Context

This closes the loop the whole project is for. Data Scout keeps layers 0–5, quota, the
`email_verifications` table, and suppression; this service does 6–8 from the isolated IP. The change
lives in **both repos** — coordinate. Data Scout invariant 10 (provider timeout + per-domain cache)
must hold. Contract: `service/storage-contract.md`, `06-generated/api.md`.

## The contract blocker is closed (2026-09-05)

Reconciled 2026-09-04 and **fixed on the Data Scout side the same day** (`389f3ae`, *"speak the
protocol email-verifier actually publishes"*). It had been written from that plan's prose rather
than from `internal/api/probe.go` and did not match in five ways: `recipients` vs `emails`, a
missing required `domain`, `results` as a list vs a map keyed by address, batch-level vs per-result
`catch_all`/`randomiser`, and `accepted`/`rejected` vs `valid`/`invalid`. Any one of them alone made
the tier inert; all of them failed in the safe direction, so invariant 1 held by construction while
the feature delivered nothing.

Verified against the current Python: the payload is `{mx_host, domain, emails, need_catch_all}`,
`results` is read as a map, `valid`/`invalid` drive `accepted`, and `catch_all`/`randomiser` are
read per result. `domain` is derived as `addresses[0].rpartition("@")[2]` — sound, because a batch
is one domain by construction (their plan 067 groups before this point), and an empty batch returns
early rather than indexing. They also caught something this plan had not written down: `POST /probe`
sits behind our auth as well as mTLS, so a client certificate alone is a `401`; their config now
carries the token.

**Our contract did not move**, which was the right call: `06-generated/api.md` matches the handler
field for field and it is the published interface.

**The path is now open, and was verified from the caller itself (2026-09-05).** From the Pi —
`178.18.32.148`, the address the `nftables` rule names — `92.222.87.97:8443` accepts in 56 ms, the
TLS handshake completes, the server presents `CN=mail.datascoutmail.com` issued by
`datascoutmail verifier CA`, and it **asks for a client certificate** ("Acceptable client
certificate CA names"). Verification fails there only because the Pi does not yet hold the CA. No
private key was moved to run this: `openssl s_client` with no client certificate is enough to prove
reachability *and* that the boundary demands one.

The same check from a third address (`217.26.166.243`) times out rather than being refused — a drop,
not a closed port — which is what the `policy drop` default should look like from anywhere the rule
does not name.

**The authenticated round trip is done — by Data Scout, 2026-09-08, twice**, and the second is the
one that counts: from inside the `api` container, using the CD-delivered material and *their own
client code*, `_post_probe` returned a live Gmail verdict. A `curl` proves the wire; their client
proves the meaning.

Re-verified independently 2026-09-10 from the Pi host, with the client bundle and the API token
staged in `tmpfs` and removed afterwards so nothing touched that host's disk: `POST /probe` answered
**`HTTP 200` in 363 ms**, `source_ip: 92.222.87.97`,
`class: invalid` from a clean `550 5.1.1`. The same request without the bearer token answered
**`401`** — the token is load-bearing exactly as their boot validator assumes.

Then the part that matters, because a `curl` proves the wire and not the meaning: the live payloads
were fed through **Data Scout's own `_result_of`**, not through a description of it.

| Verifier said | Data Scout got |
|---|---|
| `class: invalid`, `550 5.1.1` | `accepted=False` — the only case allowed to say that |
| `class: valid`, `catch_all: true` | `accepted=True`, `catch_all=True` — `scoring.py` makes the downgrade |
| `class: no_budget` (Redis unreachable) | `connected=False`, `accepted=None` — nobody refused us, we never arrived |
| an invented future class | `accepted=None`, `blocked=True` |

**No non-answer became `accepted=False`.** The five-way mismatch existed because both sides were
checked against a document instead of against each other; they have now been checked against each
other, over the wire, with the real client and the real mapping.

## Design (Data Scout side — cross-repo)

- Replace the body of `app/core/verify/smtp_probe.probe_many` with an HTTP client to `POST /probe`.
  `set_prober` stays the seam, so every existing fake prober and test keeps working. The provider
  layer, `engine.verify_many`, the domain grouping (plan 067) and the timeout/cache contract (Data
  Scout invariant 10) are all unchanged.
- **A transport failure maps to `ProbeResult{connected: false}`** — never `accepted: false`.
  Invariant 1 governs this hop as it governs SMTP; a redeploy of the probe mid-request must not be
  able to condemn a mailbox.
- Map this service's `{status,...}` onto `email_verifications.status` using the reconciliation from
  005. Store `source_ip` in `signals` — a verdict is bound to the IP that produced it.
- Retire `app/core/verify/smtp_probe.py` and its in-process ceilings (superseded by this service's
  central bucket); keep its semantics as tests/reference.
- Bulk: Data Scout's verify job calls `/verify/bulk` and polls; results persisted as today.

## Design (this service side)

Most of the work is on Data Scout's side. What this repository owes the cut-over:

- ~~**mTLS material and the switch to requiring it.**~~ **Done, plan 013.** A private CA on the
  host, a server certificate carrying `DNS:mail.datascoutmail.com` and `IP:92.222.87.97`, a client
  bundle for Data Scout at `/etc/verifierd/tls/client/`, and `client_ca_file` set so the handshake
  requires one. Proven in all four states: no certificate → `tlsv13 alert certificate required`,
  foreign CA → `unknown ca`, certificate without the API key → `401`, both → `200`. **Delivering
  the bundle** to Data Scout's CD secrets as base64 PEM is what remains, and it is one command per
  file — `operations/deployment.md` in that repository carries them.
- **The host filter narrowed to the caller — `nftables`, 2026-09-05.** The `ufw` this plan kept
  citing was never enforcing anything: `ufw status` reads `active` from its own config file, and
  neither `nft` nor `iptables` was installed, so it could not program the kernel. An
  `ufw allow from <caller> to any port 8443` would have printed nothing and changed no packet —
  failing precisely the way this bullet warns a pinned address would fail. The traffic was being
  dropped by **OVH's edge**, off the box. See `changelog.md`, 2026-09-05.

  There is now a real host filter: `inet filter`, input `policy drop`, `8443/tcp` from the caller's
  address alone, `22/tcp` unrestricted, output `accept` so outbound `:25` is untouched.

  **The open question stands, and only its urgency changed.** The caller is a Pi on a consumer line
  (the line whose ISP blocks `:25` — the reason this service exists), so its address may rotate, and
  the rule naming it would then fail silently. Still to decide: mTLS as the real boundary with a
  stable address in front, or a tunnel. What is settled is that the decision is now enforced in two
  places — this host's ruleset and OVH's edge — and **the edge is the one that still blocks the
  round trip**, because it is configured in a panel rather than in either repository.
- **Response completeness confirmed against what Data Scout stores** — `connected`, `accepted`,
  `catch_all`, `randomiser`, `smtp_code`, `enhanced_code`, `class`, `retry_after_seconds`,
  `source_ip`, `checked_at`.

## Tasks

- [x] **Data Scout: reconcile the wire contract** — done `389f3ae`; `073`'s description corrected
      in the same change, so it cannot re-seed the mistake
- [x] **Both: one round trip against the real handler before any rollout** — done 2026-09-08 by
      Data Scout from inside the `api` container with CD-delivered material; re-verified from the
      Pi host 2026-09-10, plus the mapping check above
- [x] Data Scout: `smtp_probe.probe_many` → HTTP client over mTLS; `set_prober` seam preserved
- [x] Data Scout: transport failure → `connected:false`, asserted by test — their
      `test_verify_smtp_probe.py` is green, 45 passed
- [ ] Data Scout: status reconciliation + store `source_ip`
- [x] Data Scout: retire `smtp_probe.py` in-process probing (keep as reference/tests)
- [ ] Data Scout: the Celery task keeps driving the chunk loop; it calls `/probe` once per domain
- [ ] **Data Scout owns the greylist retry** (plan 006). A `class:deferred` row is re-queued using
      the existing Celery backoff, scheduled by `retry_after_seconds` rather than by blind
      exponential backoff — the first blind attempt lands seconds later, before the window opens,
      and burns a token to be told the same thing. Exhausted retries are `unknown` with the server's
      own words, never `invalid`.
- [ ] **Data Scout keeps the retry on the same tuple.** Greylisting keys on
      `(sender, recipient, IP)`: a retry from a different node or with a different `MAIL FROM` is a
      new tuple and restarts the window. Automatic with one node; a routing constraint the moment
      there are two.
- [x] This service: a small CA, a server certificate, a client certificate for the API, and
      `tls.client_ca_file` set so one is required, with a `curl` proven through it — plan 013
- [x] This service: the host filter allows the API's address only — `nftables` 2026-09-05,
      persisted and verified. **`ufw` was never enforcing**; see the design note above
- [x] **OVH's edge opened for the caller on `8443`** — confirmed 2026-09-10 by the round trip
      itself: the Pi reaches the port, any other address still times out. Outbound `:25` survived
      it — the same round trip drove a live Gmail session, and `ExecStartPre` handshakes Gmail on
      every restart
- [x] This service: response completeness confirmed against Data Scout's `ProbeResult` mapping —
      by running live payloads through their `_result_of`, not by comparing field lists
- [ ] Both: integration test of the end-to-end path
- [ ] Docs both repos: Data Scout providers doc + this `changelog.md`

## Definition of Done

- [x] A recorded request/response pair from the live handler, matching `06-generated/api.md`
- [ ] Data Scout's verify endpoint returns this service's verdict end-to-end (staging) — recorded
- [ ] Layers 0–5 still short-circuit before any call here (no wasted probes) — test
- [ ] `source_ip` stored with each verdict — test
- [ ] Both repos' gates green; pr-checklists confirmed
- [ ] Status → Complete, moved, ROADMAP updated; Data Scout changelog entry too

## Status of the cut-over (2026-09-10) — it is live, and this plan was the stale half

Checked against Data Scout rather than assumed. **The tier is on.** `VERIFY_SMTP_ENABLED=true` since
2026-09-08 and the warm-up ladder is running at its first step, 2,000/day. Their manual test steps
2, 3, 4, 7, 8 and 9 have passed against the live deployment — step 8 (greylisting) passed in
production without needing a greylisting domain, because a throttling MX handed out a 900-second
hint during the first warm-up batch and all five re-queued rows came back within seconds of it.
Step 7 exposed and fixed a real defect on their side: an unreachable verifier was returning a
billable `valid`, and now returns `unknown`.

**Their step 6 does not pass, and the reason is work for *this* repository.** The finder cannot
confirm an address on Microsoft 365, because M365 defeats a candidate ladder twice over:

1. A wrong candidate answers `550 5.4.1 Access denied` — anti-harvesting, correctly classed here as
   `ClassPolicy` and never `invalid`. But policy-stop counts **consecutive** policy replies, and a
   ladder of wrong candidates produces exactly that, so the session is abandoned before the right
   candidate is reached. The guard is behaving as designed against a case the design did not have
   in mind: per-recipient anti-harvesting is not the same fact as a server refusing the client.
2. The second recipient in a session answers `452 4.5.3 Too many recipients` with a 900s hint.
   Verified against our own classifier: that reply is `ClassThrottled` and `IsThrottle()` is true.
   **So the pacer is told the rate is wrong when the truth is the session is full**, and the loop
   moves on to the next recipient in the same session, which answers the same thing. One batch of
   six therefore yields five throttle observations, halving the Microsoft rate five times, and five
   addresses come back `unknown` that a fresh connection would have answered.

Live metrics from the node confirm the mechanism is already firing, at low volume for now:
`verify_smtp_replies_total{code="452",class="throttled"} 4` over five days at 2,000/day. The ladder
goes to 30,000.

Neither is a bug in the classifier — both replies are labelled correctly. The gap is that **this
service has no notion of a per-MX limit on recipients per session**, and no way to say "that
rejection was about this recipient, not about us" to the policy-stop counter. Worth its own plan;
it should not be smuggled into a cut-over plan.

## Notes / decisions / deviations

Cross-repo plan — mirror an entry in Data Scout's `docs/08-decisions/changelog.md`. After this,
production has zero outbound `:25` from the main host (the reputation goal is met).
