# Plan 013 — deployment

**Status:** Complete (2026-09-05)
**Phase:** B
**Depends on:** 000, 001, 003, 009

## Goal

Deploy the service on the real isolated-IP host with a correct sender identity, reachable from Data
Scout over an authenticated link, gated on the preflight passing GO.

## Context

The host is a VPS with open outbound `:25` (build-vs-buy §3.1 names Hetzner/OVH as the ones that
actually allow it). Sender identity setup is the RUNBOOK Phases 0–3; `scripts/preflight.sh` (ported
in 000) is the go/no-go gate. The deployment form is a static binary under `systemd` plus a distro
Redis — no container runtime (ADR-005). Component:
`operations/deployment.md`.

## Design

### Host selection — settle this before anything else

- **OVHcloud VPS-1** (2 vCore / 4 GB / 40 GB NVMe, IPv4 included) or **Hetzner CX23**. Debian 13,
  64-bit, plain image — no control panel, no "ready-to-go" mail appliance.
- **Never an OVH Local Zone.** A VPS there cannot reach *any* SMTP port and it cannot be unblocked
  on request — the project would be dead on arrival with no diagnosis path. Pick a full region
  (Frankfurt / Gravelines / Warsaw).
- Hetzner blocks `:25` until the first invoice is paid (~30 days) plus a support ticket; OVH's full
  regions have it open by default. Confirm on the box before anything else:
  `timeout 8 bash -c 'exec 3<>/dev/tcp/gmail-smtp-in.l.google.com/25 && echo OPEN' || echo BLOCKED`
- **Check the IP — and its `/24` — for blocklisting before building anything on the host.** Recycled
  VPS addresses arrive pre-listed; destroying and recreating the VPS for a fresh address is far
  cheaper than delisting. Large parts of OVH space sit in UCEPROTECT L3, which some receivers
  consult, so a clean Spamhaus result alone is not the whole answer — also do a live handshake
  against Microsoft's MX and check for a pre-`MAIL FROM` policy block.

### Host setup

- Full RUNBOOK Phase 1 sender identity: rDNS `mail.<domain>` + matching FCrDNS A record, SPF
  (`ip4:<ip>`), DKIM, DMARC. HELO / `mail_from` config point at these names.
- **Identity split** (ARCHITECTURE §"Sender identity"). HELO must FCrDNS to the connecting address;
  `MAIL FROM` need not, so verification and relay use different envelope domains:

  | | HELO | `MAIL FROM` |
  |---|---|---|
  | Verification (A/B) | `mail.datascoutmail.com` | `verify@probe.datascoutmail.com` |
  | Relay (C) | `mail.datascoutmail.com` | `noreply@datascoutmail.com` |

  `probe.` carries its own SPF — a receiving MX checks SPF of the `MAIL FROM` domain against the
  connecting IP during a `RCPT` probe, so without it every probe fails SPF. This keeps probing
  reputation off the root domain that will send real mail to customers in Phase C.

  **SPF is necessary but not sufficient, and the sub-domain is not usable yet.** A strict receiver
  also asks whether the envelope sender could take a bounce. `probe.datascoutmail.com` has only the
  TXT, no MX and no A, so `gammait.net` answered `554 5.1.8` and the probe classed as `unknown`
  (plan 001). **Deployment task: give `probe.` an MX pointing at the same Cloudflare routers as the
  root** — already verified to answer `250` to `RCPT`. Until that record exists, `mail_from` stays
  `verify@datascoutmail.com` and the identity split above is aspirational, not live.
- **IPv4 only** — the host is dual-stack; the published identity covers the IPv4 address only. The
  prober pins `tcp4` (plan 001). Do not publish an IPv6 identity without deciding to.
- **No inbound MTA.** Purge anything listening on `:25` (Debian images may ship `exim4`);
  `ss -tlnp | grep ':25'` must come back empty. This service is an outbound client in Phase A/B and
  an outbound sender in Phase C — it never receives mail, and a stray Postfix on `:25` is an
  open-relay risk that burns the IP faster than any probing mistake.
- Host firewall: inbound `22` and the API port only, with the API port restricted to Data Scout's
  address. Everything else denied.

### Service

- Install the CI-built static binary to `/usr/local/bin/verifierd`; unit installed verbatim from
  `packaging/verifierd.service` (000); config at `/etc/verifierd/verifierd.yaml`; secrets via
  `EnvironmentFile` — never in the unit, never in the repo (invariant 10).
- Dedicated unprivileged `verifierd` user; `StateDirectory=verifierd`.
- Redis **with AOF persistence** (`appendonly yes`, `appendfsync everysec`) — stock Debian ships RDB
  snapshots only, which plan 006's retry queue cannot survive. See `redis-contract.md`.
- Redis from the distro package, **not reachable over the network**: `port 0` +
  `unixsocket /run/redis/redis-server.sock` (+ `unixsocketperm 660`, `verifierd` in the `redis`
  group). If the RESP client ported in 003 is TCP-only, `bind 127.0.0.1` instead — see tech-debt.
- Rollback = keep the previous binary alongside and `systemctl restart`.

### Boundary and gate

- **mTLS/API key** link from Data Scout to this host (private network or authenticated public
  ingress). No unauthenticated surface except health (invariant 11).
- **Deploy gate:** `scripts/preflight.sh <domain> mail.<domain> <dkim-selector>` must return GO on
  the real host before the service takes traffic. Wire it as a systemd `ExecStartPre` and as a CI
  deploy gate.
- One-probe topology (ADR-004); document the "add node #2" steps (new IP + rDNS + SPF entry +
  `probeN` sub-domain) without implementing them.

## Tasks

- [x] Provision the host — full region, **not** a Local Zone; confirm outbound `:25` OPEN
- [x] Verify the IP and its `/24` are not blocklisted; re-provision if they are
- [x] Host sender identity: rDNS/FCrDNS, SPF/DKIM/DMARC (RUNBOOK Phase 1)
- [x] Purge any inbound MTA; confirm nothing listens on `:25`; firewall inbound to 22 — **the API
      port is deliberately left closed**, see the note below
- [x] Install binary + `packaging/verifierd.service`; `verifierd` user; secrets via EnvironmentFile
- [x] Redis from the distro package on a unix socket / loopback; no network exposure
- [x] Redis AOF enabled and verified (`CONFIG GET appendonly` → yes)
- [x] mTLS material issued and the boundary proven from the host; **wiring the caller is plan 008**
- [x] Preflight as `ExecStartPre` — **CI deploy gate not built**, see the note below
- [x] Update `operations/deployment.md`, `SECURITY.md`, changelog

## Definition of Done

### Sender identity — verified 2026-08-28 ✅

Node: OVH VPS-1, France, Debian 13, `92.222.87.97`, domain `datascoutmail.com`.

| Check | Result |
|---|---|
| FCrDNS | `92.222.87.97` ⇄ `mail.datascoutmail.com` — both directions agree |
| SPF `@` and `probe` | `v=spf1 ip4:92.222.87.97 -all` |
| DMARC | `v=DMARC1; p=none; sp=none; rua=mailto:postmaster@datascoutmail.com` |
| DKIM | selector `s1`, RSA-2048, key at `/etc/verifierd/dkim/s1.private` (0600, `verifierd`) |
| Blocklists | Spamhaus ZEN, SpamCop, UCEPROTECT L1/L2 — clean. `/24` sampled, 0 listed. |
| UCEPROTECT L3 | listed — **ASN-wide (AS16276), not this IP**; unfixable by re-provisioning, and empirically not enforced by the providers tested below |
| Gmail live `RCPT` | `250 2.1.0 OK` on `MAIL FROM`, `250 2.1.5 OK` on `RCPT`; banner confirms `[92.222.87.97]` |
| Microsoft live `RCPT` | `250 2.1.0 Sender OK`, `250 2.1.5 Recipient OK` |
| `DATA` sent | never — `swaks -4 --quit-after RCPT` |
| Outbound `:25` | open (Gmail + Outlook); inbound `:25` — no listener, no MTA installed |
| DMARC `rua=` mailbox | live — Cloudflare Email Routing forwards `postmaster@datascoutmail.com`; end-to-end delivery confirmed |
| Root SPF | merged to a single record: `v=spf1 ip4:92.222.87.97 include:_spf.mx.cloudflare.net -all`. Email Routing wanted to add its own — two `v=spf1` records on one name are a `PermError`, not a merge |

Both live checks used `-4`. Without it the session leaves over IPv6 from an address with no FCrDNS
and no SPF — see plan 001 Notes.

### Remaining — closed 2026-09-05

- [x] `scripts/preflight.sh` returns GO on the real host — `PASS=15 WARN=2 FAIL=0`. It took three
      fixes to the script to be worth recording; see Results.
- [x] `ss -tlnp` shows no inbound listener on `:25`; Redis not bound to any public interface
      (`port 0` + unix socket)
- [x] `systemctl restart verifierd` drains in-flight sessions and comes back healthy — the drain is
      covered by `TestRunServesThenDrainsOnCancel` and the live stop logs `stopped cleanly`; coming
      back healthy needed a `Type=notify` fix, see Results
- [ ] ~~Service reachable from Data Scout over mTLS/API key; health green~~ — **reassigned to plan
      008.** Everything this repository owes is done and proven from the host: mTLS required, both
      failure modes are TLS alerts, `401` without the API key. The other half needs the caller,
      which is another repository and another machine, and 008 already carries it
- [x] A real probe against a real MX returns a correct verdict from the isolated IP and
      `source_ip` equals the host's egress (the endpoint is `POST /probe`; this plan predates the
      rename in ADR-006)
- [x] gates clean; pr-checklist confirmed
- [x] Status → Complete, moved, ROADMAP updated

### Two things deliberately not done, and why

- **The API port is closed at the firewall.** `ufw` permits `22/tcp` only. The plan says "restricted
  to Data Scout's address", and that address is the open question: the caller is a Pi on the
  consumer line whose ISP blocks `:25` — the line this service exists to route around — so it is
  probably dynamic, and a rule pinned to it would fail the day it rotates, silently, looking
  configured. Recorded as plan 008's decision; the service binds `0.0.0.0:8443` so opening it is one
  `ufw` rule.

  > **Corrected 2026-09-05, and the correction matters more than the item.** Every sentence above
  > that names `ufw` was describing a firewall that did not exist. `ufw status` reported `active`
  > because it reads that from its own config file; neither `nft` nor `iptables` was installed, so
  > it had never programmed the kernel, and `ufw.service` was `inactive (dead)`. The host had **no
  > packet filter**. What dropped the traffic was OVH's edge, off the box. The port is now genuinely
  > filtered by an `nftables` ruleset (`inet filter`, input `policy drop`, `8443/tcp` from the
  > caller's address alone), persisted and enabled. See `changelog.md` 2026-09-05 and plan 008.
  > This sign-off checked what a tool *said* instead of what the kernel *did* — the same mistake the
  > preflight bugs above were about, made one layer down.
- **No CI deploy gate.** `ExecStartPre` is wired and runs on every start. The CI half needs a deploy
  workflow with SSH credentials as repository secrets — a production deploy pipeline is its own
  decision, not a side effect of this plan, and nothing has been pushed. `ci.yml` already builds and
  publishes the static binary, so the workflow has something to deploy when that decision is made.

## Results (2026-09-05)

Node: OVH VPS-1, Debian 13, `92.222.87.97`. `verifierd` enabled and running under `systemd`,
Redis on `/run/redis/redis-server.sock` with `port 0`, `appendonly yes`, `appendfsync everysec`.
Gate green: 14 packages with `-race`, `vet`, `gofmt`, `golangci-lint` 0 issues.

**Much of the host was already correct** from the sender-identity session: the `verifierd` user
exists and is in the `redis` group, Redis is already configured as the plan wants, there is no MTA
and nothing on `:25`. What this plan added is the service itself, the boundary, and the gate.

**The deploy gate was measuring the wrong host, and would have blocked the deploy.** Its first run
returned NO-GO on a healthy node. Three separate bugs in `scripts/preflight.sh`, all failing in the
same direction — reporting something other than what it measured:

1. It found the egress with `curl https://ifconfig.me`, which on a dual-stack host answers over
   IPv6. So it graded the IPv6 identity — no PTR, no SPF — and declared FCrDNS broken. This is
   invariant 3 catching the *tool built to check invariant 3*. Fixed by forcing IPv4 at every step
   that picks a path: the egress lookup, the three `:25` dials (bash `/dev/tcp` has no `-4`, so it
   now dials the resolved A record literally), and the live handshake (`nc -4`).
2. The DNSBL section reverses the address with `awk -F.`. Given an IPv6 address that yields a name
   nobody publishes, the lookup returns nothing, and the script reads nothing as **not listed**. It
   reported three blocklists clean for an address it had never asked about. Now guarded, and it
   says "skipped" when there is no usable IPv4 egress.
3. The live handshake matched the `RCPT` reply with `grep -m1 -E '^(250|550|...)'`, which hits
   `250-mx.google.com at your service` — the first continuation line of the EHLO greeting. It would
   have reported a clean `250` whatever the server said about the recipient, **including a `5.7.x`
   block of our IP**, which is the single thing this check exists to catch. Now it takes the last
   final-form reply before the `QUIT` acknowledgement (a final reply has a space after the code, a
   continuation a hyphen). Gmail's real answer, once read correctly: `550 5.1.1 NoSuchUser`.

**`systemctl restart` was lying about readiness.** With `Type=simple` systemd calls the unit started
when `ExecStart` forks, which is before the socket is bound: a request issued immediately after
`restart` returned was refused. Harmless by hand, fatal to the CD path plan 008 needs, which cannot
otherwise tell a healthy restart from a crash loop. Fixed properly rather than with a sleep:
`main.go` binds with `ListenConfig.Listen` **before** announcing anything, then sends `READY=1`;
the unit is `Type=notify`. `sd_notify` is a dozen hand-rolled lines over a unixgram socket, no
dependency (three tests). Binding first also turns "address already in use" into a plain startup
error instead of an asynchronous one logged after we claimed to be listening. Three consecutive
restarts, each followed immediately by a request: all `200`.

**The boundary, proven rather than configured.** A private CA on the host; the server certificate
carries `DNS:mail.datascoutmail.com` and `IP:92.222.87.97` so either addressing works.

| Attempt | Result |
|---|---|
| valid client certificate + API key | `200` |
| valid client certificate, no API key | `401` |
| no client certificate | `tlsv13 alert certificate required` |
| certificate from another CA | `tlsv1 alert unknown ca` |

The last two are TLS alerts, so the request never reaches a handler — which is what 073's manual
test 1 asks for ("the handshake fails, not a 401") and what invariant 11 means.

**End to end from the isolated IP.** `POST /probe` against `gmail-smtp-in.l.google.com` returned
`source_ip: 92.222.87.97` — the host's real egress — with `class: invalid` from a clean
`550 5.1.1`, and against our own `datascoutmail.com` (Cloudflare routers) `accepted: true`,
`catch_all: true` from a `250 2.1.0`.

**That last answer exposed a documentation defect, recorded but not fixed here.** It came back
`class: "valid"` on a domain flagged `catch_all: true`. Invariant 7 says a `250` on a catch-all *is*
`risky`, and four documents name `risky` as a value this service emits — but `ClassRisky` does not
exist in `internal/prober/classify.go` and never has. Nothing is wrong end to end, because
`catch_all` travels alongside and Data Scout scores it; the exposure is a consumer that reads
`class` and ignores `catch_all`, which is precisely what the invariant exists to prevent. Left to
tech-debt with the two ways to close it, because a deployment plan is the wrong place to change a
published contract and plan 008 is about to reconcile against this exact shape.

**Two protections are dark on the node**, by configuration and visibly in the startup log: blocklist
self-monitoring (plan 010) is off because no DNSBL-capable resolver is configured — the host's stub
cannot answer DNSBL and the major zones refuse public resolvers, so it needs a local recursive
resolver — and the local suppression copy (plan 011) is off, leaving Data Scout's list as the only
one. Both match their plans' designs, which make these opt-in; both are stated in `SECURITY.md`
rather than left to be discovered.

## Notes / decisions / deviations

Do not skip the preflight gate — a deploy onto a blocked or reverse-DNS-broken IP produces verdicts
that measure nothing (RUNBOOK). GO is a hard precondition, not advisory.

No container runtime on the host — ADR-005. The `systemd` sandboxing directives in
`packaging/verifierd.service` cover the hardening a container would have given.

Checking the IP's reputation *before* building anything on the host is the step most likely to be
skipped and the most expensive to skip. It is RUNBOOK Phase 2, and it comes before Phase 3 on
purpose.

Two traps found while doing this for real, both silent:

- **A proxied `mail.` record breaks FCrDNS.** Cloudflare defaults new A records to proxied, which
  resolves the name to Cloudflare's addresses instead of the host's. The `mail.` record must be
  "DNS only". Everything else on the zone may be proxied freely.
- **A dual-stack host leaves over IPv6 by default**, bypassing the entire published identity. See
  plan 001 Notes; this is why the prober pins `tcp4`.
