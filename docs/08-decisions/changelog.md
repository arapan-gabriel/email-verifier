# Changelog

One entry per plan (always), newest first: decisions made, deviations, library/provider choices,
trade-offs.

## 2026-08-28 — Invariants renumbered 1–11; IPv4-only promoted to a hard invariant

- **`CLAUDE.md`'s hard-invariant list is now 1–11 with no gaps or letters.** The old list ran
  `1, 2, 2b, 3 … 9`, and `AGENTS.md` mirrored it with the `2b` flattened away — so the two files
  disagreed on every number from 3 onward, and two docs had already drifted onto the wrong one:
  `patterns/smtp-classification.md` and plan 010 both cited "invariant 5" for policy-≠-throttle,
  which was 4 in `CLAUDE.md` and 5 only in `AGENTS.md`. Both corrected.
- **IPv4-only is now invariant 3**, promoted from a plan-level note. It sits next to the SSRF guard
  because the two are the same question from opposite ends: guard 2 governs which address we connect
  *to*, invariant 3 governs which address we connect *from*.
- Old → new: `2b`→4, 3→5, 4→6, 5→7, 6→8, 7→9, 8→10, 9→11. Invariants 1 and 2 unchanged. Every
  citation across `docs/` was remapped in the same change; `Data Scout invariant 10` references
  belong to the other repo's numbering and were deliberately left alone.
- `AGENTS.md` now states that it only mirrors `CLAUDE.md` and the two renumber together — the
  ambiguity that caused the drift.
- `pr-checklist.md` gains IPv4-only to its mandatory block (now five items, not four);
  `SECURITY.md` gains the matching line.

## 2026-08-28 — Sending node provisioned; sender identity live; IPv4-only pinned

- **Node deployed and verified.** OVH VPS-1 (2 vCore / 4 GB, France), Debian 13, `92.222.87.97`,
  domain `datascoutmail.com`. Outbound `:25` open, no inbound MTA, SSH key-only, ufw (inbound 22,
  **outbound allowed** — a `deny outgoing` default would silently kill port 25), chrony synced,
  unattended-upgrades on, Redis on a unix socket with TCP disabled (`port 0`), `verifierd` user and
  directories created. Full state and results recorded in plan 013's Definition of Done.
- **Sender identity published and confirmed against live MXes.** FCrDNS agrees both ways; SPF on the
  root and on `probe.`; DMARC with `sp=none`; DKIM selector `s1` (RSA-2048). Gmail and Microsoft
  both answered `250 2.1.5` on `RCPT` with no policy block. No `DATA` was ever sent
  (`swaks -4 --quit-after RCPT`).
- **New architectural invariant: IPv4 only.** Measured, not theoretical — on this dual-stack host a
  plain `net.Dial("tcp", ...)` chose IPv6 and reached Gmail from `2001:41d0:404:200::169b`, an
  address with the provider-default PTR and no SPF coverage. Providers accept the connection and
  reject at `RCPT` with `5.7.x`, which this service classifies as `ClassPolicy` — so every probe
  would return `unknown` while looking like a classifier defect. Plan 001 now pins `dial_network:
  tcp4` as an explicit config value with a test; recorded in `ARCHITECTURE.md` §Invariants.
- **Sender-identity split recorded** (`ARCHITECTURE.md` §"Sender identity", plan 013,
  `operations/dns.md`): HELO is always `mail.<domain>` (it must FCrDNS), while `MAIL FROM` differs —
  `verify@probe.<domain>` for verification, `noreply@<domain>` for Phase C relay. `probe.` carries
  its own SPF because a receiving MX checks the `MAIL FROM` domain's SPF against the connecting IP
  during a `RCPT` probe. This keeps probing reputation off the domain that will carry customer mail.
  The RUNBOOK, ported from the lab, still assumes a single `verify@yourdomain.com` for everything.
- Two silent traps documented in plan 013 Notes and `operations/dns.md`: a CDN-proxied `mail.` A
  record breaks FCrDNS (Cloudflare proxies new A records by default), and a dual-stack host leaves
  over IPv6 unless the address family is pinned.
- UCEPROTECT L3 lists the IP, but ASN-wide (AS16276 / OVH), not the address; Spamhaus ZEN, SpamCop
  and UCEPROTECT L1/L2 are clean, the `/24` sampled clean, and both providers tested accept the
  session. Re-provisioning would not clear an ASN-level listing — recorded so it is not re-raised.
- Still open: Email Routing for the DMARC `rua=` mailbox (MX currently empty, so reports are lost),
  and `scripts/preflight.sh` as the formal gate — the script is ported in plan 000, so the checks
  above were run by hand.

## 2026-08-26 — Deployment form: systemd, no container runtime

- **ADR-005 accepted** — deploy as a systemd service, not a container. Plans **000** and **013**
  rewritten to match. The release artifact is a single `CGO_ENABLED=0` static
  binary installed under `systemd` (`packaging/verifierd.service`), with Redis from the distro
  package on a unix socket or loopback. The original `Dockerfile` + `docker-compose.yml` tasks are
  dropped, along with the `docker compose up` item in 000's Definition of Done.
- Reasons are project-specific, not stylistic: Docker's bridge SNAT would mask the egress address
  that `source_ip` must report; `172.16/12` is exactly what the SSRF guard rejects (invariant 2);
  and a bridge hop on the token-bucket path adds a failure mode to a path that is fail-closed by
  invariant 5. A `CGO_ENABLED=0` binary has no dependencies to isolate.
- `systemd` sandboxing (`ProtectSystem=strict`, `NoNewPrivileges`, empty `CapabilityBoundingSet`, …)
  replaces the hardening a container would have provided; `LimitNOFILE=65535` covers the concurrent
  SMTP session count from ADR-001.
- Plan 013 gained concrete host guidance from the provider survey: full region only (an OVH **Local
  Zone** can never reach any SMTP port, with no unblock path), IP and `/24` reputation verified
  before the host is built on, plain Debian 13 with no control panel, and no inbound MTA.
- Plan 013 now also depends on 000 (it installs 000's `packaging/verifierd.service`).
- Deferred to tech-debt: unix-socket support in the ported RESP client.
- ADR-005 explicitly leaves ADR-004 intact: a static binary is identical across nodes by
  construction, so "add node #2" stays a deployment step.

## 2026-08-26 — Repo bootstrap & architecture

- Created `email-verifier` as a standalone Go service repo with the Data Scout docs layout.
- **Architecture locked** via ADRs 001–004:
  - ADR-001 — build on the `ds-smtp-retry` Go engine (mature AIMD/bands/classifier/central bucket).
  - ADR-002 — scope = verification probe + outbound relay, phased (verify first, relay in Phase C);
    layers 0–5 stay in Data Scout.
  - ADR-003 — HTTP integration now (queue later); stateless about business data (Data Scout stores
    verdicts).
  - ADR-004 — one probe node at the start; the token bucket is central from plan 003 so scaling is
    deployment-only.
- Seeded docs: `CLAUDE.md`, `README.md`, `ARCHITECTURE.md`, `ENGINEERING-STANDARDS.md`, `AGENTS.md`,
  `DOCS_GUIDE.md`, product index, generated contracts (`api`/`redis-contract`/`metrics`), patterns
  (`smtp-classification`/`aimd-pacing`/`ssrf-guard`/`retry-greylist`), pr-checklist, ROADMAP, plan
  template, and plan 000.
- Ported references from `ds-smtp-retry`: `RUNBOOK.md`, `token_bucket.lua`; and the Data Scout
  build-vs-buy analysis (the decision record mandating a separate isolated-IP probe).
- No code yet — execution starts at plan 000 (scaffold-and-standards).
