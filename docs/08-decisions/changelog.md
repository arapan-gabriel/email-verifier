# Changelog

One entry per plan (always), newest first: decisions made, deviations, library/provider choices,
trade-offs.

## 2026-08-28 — Plan 001: the prober, and `POST /probe`

- **Classifier ported verbatim** from `../ds-smtp-retry/ratecheck/internal/prober` into
  `internal/prober/classify.go` — the reply→class map, `IsTemp`/`IsThrottle`, the RFC 3463 reader
  and the hint lists, unchanged apart from doc comments the linter required. It is deliberately kept
  close to upstream so measurements made in the lab still describe this code, and it is the single
  place in the service where a code becomes a meaning.
- **New batched session** (`internal/prober/prober.go`): connect → EHLO → MAIL FROM → RCPT × N →
  one bogus RCPT for catch-all → RSET → QUIT, per ADR-006. `DATA` is never sent (invariant 8).
  Batches split at `max_rcpt_per_session`; catch-all is probed once per request because it is a
  property of the domain, not of a chunk.
- `Connected`/`Accepted`/`CatchAll` are tri-state pointers marshalling to `null`, mapping one-to-one
  onto Data Scout's existing `ProbeResult`. **Only `ClassValid` and `ClassInvalid` set `Accepted`;**
  every other class leaves it nil, which is how invariant 1 is carried in the type rather than in a
  convention someone has to remember.
- **Invariant 3 is now enforced, not documented.** `probe.dial_network` must be exactly `tcp4` or
  the service refuses to boot; `tcp`, `tcp6`, `udp` and empty are all rejected, with a test.
- `POST /probe` behind `Options.Authenticated`, so the route cannot be registered without the guard.
  The handler declares its own one-method `Prober` interface (ENGINEERING-STANDARDS §2), so the API
  tests use a three-line fake and open no sockets.
- **mTLS implemented but not enabled**: `tls.{cert_file,key_file,client_ca_file}` →
  `RequireAndVerifyClientCert` with TLS 1.3 minimum. The CA and certificates come with plan 013;
  until then the API key guards the route and startup warns about both plain HTTP and
  TLS-without-client-certs. A half-configured listener (cert without key, CA without cert) is a
  boot refusal, because it would otherwise serve plain HTTP on a port the caller believes is
  protected.
- **`auth.enabled` now defaults to true.** An authenticated route exists from this plan on, so the
  edge is guarded by default and the service will not boot without a key (invariant 11).
- Integration tests drive `internal/mxsim` in-process on an ephemeral port: gmail profile gives
  `valid`/`invalid` correctly and `catch_all:false`; the catch-all profile gives `catch_all:true`;
  the 11th connection in the rate window returns `class:throttled` with `connected:false` and
  `accepted:null`. That last one is the whole point — the same `421`, arriving in the banner before
  `MAIL FROM`, must be simultaneously "not a verdict about this mailbox" and "the one signal that
  moves the pacer".
- Found while testing: the teardown `RSET`/`QUIT` could block on a server that stops reading. The
  answers are already collected by then, so both writes now get a short write deadline — a tarpit
  must not be able to hold a session, and its slot in the rate budget, open for the full timeout.
- `probe.port` added (25 in production) so a staging instance can be aimed at the lab MX.
- **First run against real MXes found an invariant-1 violation in the ported classifier.** A
  `554 5.1.8` about our envelope sender marked two live mailboxes `invalid`: RFC 3463 puts `X.1.7`
  and `X.1.8` in the "addressing" subject next to the recipient codes, but they are about the
  *sender*, and `classifyPermanent` read all of subject 1 as a statement about the recipient. Both
  added to `senderCodes` (checked first), sender wording added to the hints, regression test added.
  **`../ds-smtp-retry` still carries this bug** — port the fix back.
- **`probe.datascoutmail.com` is not a routable sender domain.** It carries only the SPF TXT record;
  a server doing sender-domain verification finds no MX and no A and rejects every probe with
  `5.1.8`. Confirmed by switching to `verify@datascoutmail.com`, which the same server accepts. The
  fix is an MX on the sub-domain pointing at the same Cloudflare routers as the root — verified that
  Cloudflare answers `250` to a `RCPT` for it, so sender callouts pass and not just DNS lookups.
  Until then the sub-domain isolation from ARCHITECTURE §"Sender identity" is not in effect.
- **Consumer `gmail.com` does distinguish a real mailbox from a missing one.** Measured: our own
  address `250 2.1.5`, a random local part `550 5.1.1 NoSuchUser`. `build-vs-buy` §4 assumes Gmail
  "answers identically to an existing and a non-existing mailbox" and prices the project on that
  assumption. That is not what consumer Gmail did here. Google Workspace-hosted domains are a
  separate case and were not tested — but §4's estimate of what a self-hosted probe is worth looks
  too pessimistic and should be re-checked before it is used for planning again.
- **Signed off 2026-08-28.** Plan 001 is Complete and moved to `completed/`. Phase A continues at
  plan 002 (ssrf-guard-and-safety), which is what makes the endpoint safe to expose at all.

## 2026-08-28 — ADR-006: the seam is `probe_many`; no jobs here; Redis persistence required

Three corrections found by reading the Data Scout side properly before starting plan 001, rather
than after plan 008 would have forced them.

- **ADR-006 accepted**, superseding ADR-003's transport shape and bulk handling. Its
  "stateless about business data" decision is unchanged and carried further.
- **The seam is `smtp_probe.probe_many`, not the provider.** Data Scout already resolves MX and
  groups addresses by domain (its plan 067, done deliberately so "500 addresses no longer mean 500
  connections"). Cutting at `verify(email)` would have issued one HTTP request per address and
  destroyed that grouping. The contract is now `POST /probe {mx_host, domain, emails[],
  need_catch_all}` → per-address results mapping one-to-one onto the existing `ProbeResult`.
- **No jobs and no bulk endpoint on this service.** Data Scout's Celery already owns chunking,
  progress, per-row quota metering, the result artifact and a reaper for stalled jobs. Duplicating
  it here meant two definitions of "done" — and ROADMAP 007 never said what resumed an in-process
  bulk worker after a restart. With no jobs here a redeploy costs at most one in-flight chunk, which
  the caller retries. Plan 007 shrinks to policy-stop alone; plan 004 narrows to A/AAAA + the SSRF
  guard (MX discovery stays in Data Scout); plan 008 becomes a change to one function plus mTLS.
- **A transport failure between the two services is `unknown`, never `invalid`.** Invariant 1
  governs the HTTP hop exactly as it governs SMTP — a probe redeploy mid-request must not be able to
  condemn a mailbox. Recorded in ADR-006, `api.md`, `ARCHITECTURE.md` and plan 008.
- **Authentication is mTLS**, with an API key as a second factor inside the tunnel for operator
  routes. The host is on a public IP with `:25` open and is scanned continuously; mTLS ends the
  handshake before a request reaches the application.
- **Redis persistence is now a requirement, not a default.** Measured on the deployed node:
  `appendonly no`, RDB only (`save 3600 1 300 100 60 10000`), so a crash or power loss drops up to
  an hour of writes. Tolerable for calibrated bands, which are re-learned by design — **not**
  tolerable for plan 006's greylist retry queue, whose whole promise is surviving a restart. A
  losing retry means an address is never re-asked and the caller waits forever. `appendonly yes` +
  `appendfsync everysec` added to plan 006, plan 013 and `redis-contract.md`.
- Open question flagged in plan 006: with no job here to attach it to, a deferred retry has no
  synchronous caller to return to. The option most consistent with the architecture is that the
  retry queue is not this service's concern at all — return `class:deferred` and let Data Scout
  re-queue. To be resolved before that plan is implemented.

## 2026-08-28 — Coding patterns fixed in ENGINEERING-STANDARDS, and retrofitted

Chosen for what this service actually has to get right, not for novelty. Each rule is tied to an
invariant it protects, and plan 000's own code was rewritten to follow it rather than only
documenting it.

- **Errors are classified by type, never by message text** (§4). `errors.Is`/`errors.As`, sentinels,
  `%w` wrapping; `strings.Contains(err.Error(), …)` is forbidden. Invariant 1 — "a rejection of us is
  never a verdict about the address" — is carried entirely by how failures are typed, and a string
  match rots the moment a provider rewords a reply. The failure mode is deleting a live mailbox.
  Applied now: `config.ErrInvalid` plus `errors.Join` so validation reports every problem at once.
- **Interfaces are declared by the consumer, one or two methods** (§2). This is what makes the test
  seams the docs already demanded actually cheap. `api.ReadinessFunc` is the pattern at its
  smallest; plan 001's handler will own a one-method `Verifier`.
- **Dependencies are passed in; no package-level mutable state, no `init()`** (§2). ADR-004 forbids
  pacing state a second node cannot see, and a package-level variable is exactly that.
- **`main` is a wrapper around `run(ctx, args, getenv, stderr) error`** (§2). Startup, graceful
  drain and bad-config paths are now covered by tests that spawn no process.
- **`context.Context` first on anything doing IO**, never stored in a struct (§5).
- **`testing/synctest` for all time-dependent tests** (§7), GA in Go 1.25 and verified working in
  this module: six simulated minutes elapse in 0.00 s. Cooldowns, greylist backoff and bucket refill
  are asserted deterministically instead of slept through. **The lab's hand-rolled `Clock` is not
  ported into the service** — `synctest` fakes `time` itself, so production signatures stay clean.
  `internal/mxsim` keeps its own clock only because it is ported code kept close to upstream.
- Also codified: constant-time credential comparison, `Options` structs over functional options,
  `t.Context()` over `context.Background()` in tests, `getenv` injection over `t.Setenv`.
- Propagated into `testing/{strategy,unit}.md`, `pr-checklist.md` (six new review items) and the
  plans where the rules bite: 001 (typed reply classes, consumer-side `Verifier`), 003, 006 and 012
  (`synctest` for pacing, retry backoff and calibration timing).

## 2026-08-28 — Plan 000: scaffold, gates, systemd unit, mxsim ported

- **First code.** Module `github.com/arapan-gabriel/email-verifier`, Go 1.25, one dependency
  (`gopkg.in/yaml.v3`, pulled in by config parsing and the ported mxsim profiles).
- `internal/config` — the single config source: defaults → optional YAML → `VERIFIERD_*` env, then
  validation. The service refuses to boot on a bad or missing required value instead of starting
  half-configured; `auth.enabled` without `auth.api_key` is one of the refusals.
- `internal/api` — router, canonical `{"error":{"code","message"}}` shape, `GET /healthz`,
  `GET /readyz`, JSON 404 for anything else. `Options.Authenticated` is the seam plan 001 hangs
  `POST /verify` on, so no route can quietly skip the guard (invariant 11).
- `cmd/verifierd` — config load, `log/slog` JSON logging, graceful drain on SIGTERM/SIGINT.
- **`mxsim` ported** from `../ds-smtp-retry/mxsim` into `internal/mxsim/{smtp,policy,clock,metrics,
  admin}` plus `cmd/mxsim`, profiles in `config/mxsim/`. It was pulled into this plan because plans
  001, 003, 006 and 012 all name it in their Definition of Done and none of them tasked porting it —
  001 would have been blocked on unplanned work. Its tests pass unchanged in this module; it is
  linted loosely on purpose (`.golangci.yml`) so upstream fixes stay easy to apply.
- Also ported: `scripts/preflight.sh`, `config/limiter/token_bucket.lua` into the location the docs
  already cite, and the seed rate bands as `config/limits/` (71 provider files).
- `Makefile` carries the static build flags (ADR-005: the binary is the artifact); `make gate` is
  the Phase-4 gate verbatim. CI runs the same four steps and then asserts the built binary is
  statically linked.
- `packaging/verifierd.service` — hardened unit with `LimitNOFILE=65535`, SIGTERM drain inside
  `TimeoutStopSec=30s`, `RestrictAddressFamilies=AF_INET AF_UNIX` (which also happens to enforce
  invariant 3 at the kernel level), empty capability set. Verified with `systemd-analyze` on the
  real host.
- `readyz` dials the configured Redis endpoint rather than issuing a real `PING` — the RESP client
  arrives in plan 003. Recorded in `api.md` so the contract does not overstate what is checked.
- Gate green: `go test -race`, `go vet`, `gofmt -l`, `golangci-lint run`. Full results in the plan.
- **Signed off 2026-08-28.** Plan 000 is Complete and moved to `completed/`; the ROADMAP row is
  closed. Phase A execution continues at plan 001 (http-verify-service).

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
