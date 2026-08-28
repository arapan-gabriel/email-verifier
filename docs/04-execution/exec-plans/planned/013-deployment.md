# Plan 013 — deployment

**Status:** Planned
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

- [ ] Provision the host — full region, **not** a Local Zone; confirm outbound `:25` OPEN
- [ ] Verify the IP and its `/24` are not blocklisted; re-provision if they are
- [ ] Host sender identity: rDNS/FCrDNS, SPF/DKIM/DMARC (RUNBOOK Phase 1)
- [ ] Purge any inbound MTA; confirm nothing listens on `:25`; firewall inbound to 22 + API
- [ ] Install binary + `packaging/verifierd.service`; `verifierd` user; secrets via EnvironmentFile
- [ ] Redis from the distro package on a unix socket / loopback; no network exposure
- [ ] Redis AOF enabled and verified (`CONFIG GET appendonly` → yes)
- [ ] mTLS/API-key link to Data Scout
- [ ] Preflight as `ExecStartPre` and CI deploy gate
- [ ] Update `operations/deployment.md`, `SECURITY.md`, changelog

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

### Remaining

- [ ] `scripts/preflight.sh` returns GO on the real host (recorded)
- [ ] `ss -tlnp` shows no inbound listener on `:25`; Redis not bound to any public interface
- [ ] `systemctl restart verifierd` drains in-flight sessions and comes back healthy
- [ ] Service reachable from Data Scout over mTLS/API key; health green
- [ ] A single `/verify` against a real MX returns a correct verdict from the isolated IP, and the
      returned `source_ip` equals the host's real egress address
- [ ] gates clean; pr-checklist confirmed
- [ ] Status → Complete, moved, ROADMAP updated

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
