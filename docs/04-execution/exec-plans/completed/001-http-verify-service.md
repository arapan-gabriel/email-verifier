# Plan 001 — http-verify-service

**Status:** Complete (2026-08-28)
**Phase:** A
**Depends on:** 000

## Goal

Port the SMTP prober from `ds-smtp-retry` and expose it as an authenticated `POST /probe` — one
batch scoped to one recipient MX. This is the first real capability, and the shape is fixed by
ADR-006: Data Scout has already run layers 0–5, resolved the MX and grouped the addresses.

## Context

`000` gave us the server skeleton, config, CI, healthz. The engine to port is
`../ds-smtp-retry/ratecheck/internal/prober` (one RCPT session + reply classification). Pacing is NOT
in this plan (it arrives in 003) — 001 uses a single serial probe with a fixed conservative rate so
the endpoint is testable end-to-end first. SSRF guard is 002; until then the resolver is trusted only
for `.test`/lab targets in tests.

## Design

- `internal/prober` — port from the lab: connect → `EHLO` → `MAIL FROM` → `RCPT TO` → `QUIT`,
  behind a `Prober` interface so tests never open a socket (mirror `smtp_probe.py`'s `set_prober`).
  **`DATA` is never sent.**
- Classifier ported verbatim (`Classify`, `classifyPermanent`, enhanced-code reader, `IsThrottle`/
  `IsTemp`) — it is the single reply→verdict source (`patterns/smtp-classification.md`).
- **Failure classes are typed, not string-matched** (ENGINEERING-STANDARDS §4). The port must expose
  the reply class as a value tested with `errors.Is`/`errors.As` or an explicit enum — never
  `strings.Contains(err.Error(), …)`. Invariant 1 rides entirely on this distinction, and a provider
  rewording a reply must not be able to turn a blocked probe into `invalid`.
- **The handler depends on a one-method interface it declares itself** (§2):
  `type Verifier interface { Verify(ctx, VerifyRequest) (VerifyResult, error) }`. Registered through
  `api.Options.Authenticated` so the route cannot skip the guard.
- `internal/api` — `POST /probe {mx_host, domain, emails[], need_catch_all, helo?, mail_from?}` →
  `{source_ip, checked_at, results{email → {connected, accepted, catch_all, smtp_code,
  enhanced_code, class}}}` (`06-generated/api.md`). Handler is thin: parse → auth → one engine call
  → serialise.
- **One session serves the whole batch**, split at a configurable max-RCPT-per-session — an
  unbounded recipient list is itself a harvesting signal and servers commonly cap it near 100. This
  mirrors what Data Scout's `smtp_probe.probe_many` already does.
- `connected`/`accepted`/`catch_all` are **tri-state**: `null` means the server never gave a usable
  answer, which is a different fact from `false`. They map one-to-one onto Data Scout's existing
  `ProbeResult`, so the caller's code above the seam does not change.
- `need_catch_all` is decided by the caller (it owns the domain-profile cache). This service
  performs the detection; it does not decide when it is needed.
- **Auth (real):** mTLS or API-key middleware; every route but health. Replaces the 000 stub.
- Per-probe timeout from config; request context bound.
- **Dial `tcp4`, never `tcp`** (invariant 3) — an explicit config value (`dial_network`), not a
  default. The sending identity is published for IPv4 only; a dual-stack host would otherwise
  prefer IPv6 and connect from an address with no FCrDNS and no SPF. See Notes.
- `source_ip` resolved once at startup (or per config) and returned with every verdict.
- Verdict enforcement of invariant 1 lives in the classifier: only a `5.x` on `RCPT` after a good
  `MAIL FROM` yields `invalid`; everything else that fails → `unknown`.

## Tasks

- [x] Port `internal/prober` (session + classifier) with the `Prober` interface + a fake for tests
- [x] Port the enhanced-code classifier and `IsThrottle`/`IsTemp`
- [x] `POST /probe` handler + request/response structs matching `06-generated/api.md`
- [x] Batch splitting at max-RCPT-per-session; one session per chunk
- [x] Real auth middleware (mTLS/API key); wire onto all non-health routes
- [x] Config: probe timeout, HELO, MAIL FROM, source IP, `dial_network` (tcp4), auth secret
- [x] Test asserting the dialer is address-family-pinned (no bare `tcp`)
- [x] Table tests for the classifier; a `/verify` handler test with a fake prober
- [x] Update `06-generated/api.md` (mark `/verify` live) and `changelog.md`

## Definition of Done

- [x] `curl /probe` with a batch returns `accepted:true` for a known-good and `accepted:false`
      with `5.1.1` for a known-bad address, in one session, against `mxsim` (record how)
- [x] A catch-all `mxsim` profile returns `catch_all:true` for the batch
- [x] A rejection of us (timeout, 5xx before MAIL FROM, `421` in the banner) yields
      `connected:false`, never `accepted:false` — test
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [x] pr-checklist items confirmed (us ≠ address)
- [x] Status → Complete, moved to `completed/`, ROADMAP row updated

## Results (2026-08-28)

Gate: `go test -race` · `go vet` · `gofmt -l` · `golangci-lint run` — all clean.

Against `internal/mxsim` in-process (`integration_test.go`) and end-to-end over HTTP with `curl`:

| Scenario | Result |
|---|---|
| gmail profile, batch of 3, one session | `valid@` and `ceo@` → `accepted:true` `250 2.1.5`; `nope@` → `accepted:false` `550 5.1.1` |
| catch-all profile | `accepted:true` **and** `catch_all:true` — the caller is told the 250 means nothing |
| gmail profile, `need_catch_all` | `catch_all:false` — the bogus probe was rejected |
| 11th connection in the rate window | `class:throttled`, `connected:false`, **`accepted:null`** — never `invalid` |
| `POST /probe` without a bearer token | `401 {"error":{"code":"unauthorized"}}` |
| malformed body, unknown field, empty batch, over-limit batch, `need_catch_all` without domain | `400 bad_request` |
| engine failure | `502`, no `results` object rendered |

### Against real MXes from the deployed node (2026-08-28)

Run from `92.222.87.97` with the published identity, binding to `127.0.0.1` and driven over SSH —
the endpoint was never exposed, because the SSRF guard (002) and pacing (003) do not exist yet.
Volume was single-digit and every target was our own domain or our own mailbox.

| Target | Result |
|---|---|
| `gmail.com` — our own mailbox vs. a random local part | `250 2.1.5` → `accepted:true`; `550 5.1.1 NoSuchUser` → `accepted:false`; `catch_all:false` |
| `datascoutmail.com` (Cloudflare Email Routing) | both addresses `250` and **`catch_all:true`** — the caller is told the 250 means nothing |
| `outlook.com` | `250 2.1.5 Recipient OK` |
| `gammait.net` (Namecheap Private Email) | `450 4.1.1` on first contact → `class:deferred`, `accepted:null` — the greylisting path plan 006 exists for |

**The run found a real defect, which is the point of running it.** See Notes.

The throttle row is the one that matters: `421` arrives in the banner, before `MAIL FROM`, and the
same reply must be both "not a verdict about this mailbox" (invariant 1) and "the one signal that
moves the pacer" (invariant 6). Asserted in both the unit and integration suites.

## Notes / decisions / deviations

**A classifier bug found on the first real run — and it was an invariant-1 violation.**

Probing our own `gammait.net` came back `554 5.1.8 <verify@probe.datascoutmail.com>: Sender address
rejected` — the server refusing our *envelope sender* — and the classifier marked **both live
mailboxes `invalid`**. Exactly the failure this whole service is built to prevent, caught by the
first contact with a real MX.

Cause: RFC 3463 puts `X.1.7` and `X.1.8` in the "addressing" subject alongside the recipient codes,
but the RFC assigns them to the *sender* ("bad sender's mailbox address syntax", "bad sender's
system address"). The ported `classifyPermanent` reads all of subject 1 as a statement about the
recipient. Fixed by adding both to `senderCodes`, which is checked first, plus sender wording to the
hint list. Now `class:policy`, `accepted:null`.

**This is a deviation from `../ds-smtp-retry`, which still carries the bug** — worth porting back.

**An identity problem, found by the same reply.** `probe.datascoutmail.com` carries only a TXT (SPF)
record: no MX, no A. A receiving server doing sender-domain verification finds nothing and rejects
with `5.1.8`, so *every* probe fails against strict servers. Confirmed by switching `mail_from` to
`verify@datascoutmail.com` (the root, which has MX) — the rejection disappears.

Fix: give `probe.datascoutmail.com` its own MX pointing at the same Cloudflare routers as the root.
Verified that Cloudflare answers `250` to `RCPT TO:<verify@probe.datascoutmail.com>`, so sender
callouts pass too, not just DNS existence checks. Until that record exists, `mail_from` must be
`verify@datascoutmail.com` and the sub-domain isolation is not in effect.

**Deviations from the plan as written:**

- `probe.port` was added to config. It is 25 in production; it exists so a staging instance (and the
  manual gate above) can aim at `internal/mxsim` without touching anyone else's server.
- mTLS is implemented as listener configuration (`tls.cert_file`, `tls.key_file`,
  `tls.client_ca_file` → `RequireAndVerifyClientCert`, TLS 1.3 minimum) but is **not yet enabled**:
  the CA and certificates are provisioned by plan 013. Until then the API key is the guard, and the
  service logs a warning at startup for both plain HTTP and TLS-without-client-certs.
- **Invariant 3 is enforced by config validation, not documentation**: `probe.dial_network` must be
  exactly `tcp4` or the service refuses to boot. A test covers `tcp`, `tcp6`, `udp` and empty.
- A short write deadline was added around the teardown `RSET`/`QUIT`. Found by a unit test hanging:
  the answers are already collected at that point, and a tarpitting server that stops reading must
  not be able to hold the session — and its slot in the rate budget — open for the full timeout.

Fixed conservative rate here on purpose — real per-MX pacing is 003. Do not add ad-hoc semaphores
that a second node cannot see; leave pacing to the central bucket in 003.

**IPv4-only is not a preference, it is correctness** (measured on the deployed node, 2026-08-28).
The OVH VPS is dual-stack, and a plain `net.Dial("tcp", ...)` chose the IPv6 address on the first
try: Gmail was reached from `2001:41d0:404:200::169b`, whose PTR is the provider default and which
appears in no SPF record. Google accepts the connection (`220`) and rejects later with a `5.7.x`,
which this service classifies as `ClassPolicy` — so **every probe would return `unknown`** while
looking like a classifier bug rather than a network one. The same trap applies to any manual
testing: `swaks -4`, never bare `swaks`.

Either pin `tcp4`, or publish a full IPv6 identity (PTR + `AAAA` + `ip6:` in SPF). The second buys
nothing — many MX have no `AAAA` at all, and providers hold IPv6 senders to stricter rules.
