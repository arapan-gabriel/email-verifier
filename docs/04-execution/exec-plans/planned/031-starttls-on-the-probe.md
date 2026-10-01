# Plan 031 — STARTTLS on the probe

**Status:** Planned (written 2026-10-01)
**Phase:** B
**Depends on:** 002 (SSRF guard — unaffected, the upgrade runs over the vetted connection), 024 and
030 (a TLS demand is classed `policy` meanwhile)

## Goal

Speak STARTTLS when a receiving server advertises it, the way a real MTA does — opportunistically,
TLS 1.2/1.3, SNI set — so that MXes which require encryption answer the question instead of
refusing the session, and so that every session we open looks like ordinary mail transport rather
than a plaintext script.

## Context

**The prober has no TLS at all.** The session is connect → banner → `EHLO` → `MAIL FROM` →
`RCPT` × N → (catch-all `RCPT`s) → `RSET`/`QUIT` (`internal/prober/prober.go`, `session`); there
is no `STARTTLS` step and no `crypto/tls` import. The test server agrees: mxsim answers `STARTTLS`
with `454 4.7.0 TLS not available` (`internal/mxsim/smtp/server.go`). `tech-debt.md` has carried
"no STARTTLS step" since the ladder's day 3; plan 024 made the *consequence* honest (a TLS demand
is `policy`, not `invalid`) and said explicitly that it does not add STARTTLS. This plan does.

**What it costs today, measured on the warm-up ladder** (29,573 rows, 2026-09-12 → 10-01): **110
rows at 51 MX hosts** answered with a TLS demand instead of a verdict —
`530 5.7.0 STARTTLS is mandatory`, `550 5.7.1 Session encryption is required`,
`550 Must issue STARTTLS`, `550 Must use TLS`, `550 TLS encryption required for mails from
92.222.87.97`, `421 4.7.0 STARTTLS is mandatory`, and **37 at Sophos Email**
(`mx-01-eu-central-1.prod.hydra.sophos.com`): `550 5.7.4 XGEMAIL_0006 … the configured TLS version
is Preferred TLS 1.3 for the recipient … and the sender … TLS version is not available` — "not
available" is literal: we offered none. Sophos is a filtering service in front of many customers,
so this is a growing share, not a fixed list. Every such row is `unknown` at best and, before
024/030, was sometimes `invalid` or (Data Scout side) `valid`.

**Reputation, not only coverage.** A plaintext session to a server that advertises STARTTLS is
one of the cheapest bot signals there is; Google Postmaster Tools reports the share of a sender's
traffic that arrived over TLS as a reputation dimension of its own. Upgrading costs a handshake (~100-300 ms) and changes no rate.

## Design

1. **Parse the EHLO reply** (already read whole by `readReply`, plan 022) for the `STARTTLS`
   keyword. Absent → the session proceeds in plaintext exactly as today.
2. **Upgrade when advertised.** `STARTTLS` → expect `220` → `tls.Client(conn, cfg)` over the
   **already-dialled, already-vetted** connection (no new dial, no hostname resolution — invariant
   2 is untouched) → handshake under the session deadline → `EHLO` again (RFC 3207 §4.2: the
   pre-TLS extension list is discarded).
3. **The TLS policy of an opportunistic MTA (RFC 7435, RFC 3207):**
   - `ServerName` = the MX hostname (SNI), `MinVersion` TLS 1.2, Go's default suites, TLS 1.3 on.
   - **Certificate verification is recorded, never enforced**: `InsecureSkipVerify: true` with a
     `VerifyConnection` callback that runs the normal chain + hostname check and records the result
     (`verified` / `untrusted` / `name_mismatch`) instead of failing. Most MX certificates do not
     match the MX name; an opportunistic sender that refused them would refuse half the internet,
     and Postfix's default (`smtp_tls_security_level = may`) does exactly this.
   - No client certificate.
4. **A failed upgrade is about us, never about the mailbox, and never a throttle.** A non-`220`
   answer to `STARTTLS` or a handshake error ends the session with a new class `tls_failed`:
   `IsTemp`, **not** `IsThrottle` (a TLS mismatch is not our rate — invariant 6), every address
   unattempted with a retry hint (invariant 1). **No plaintext fallback in this plan**: a server
   that advertised TLS and then failed the handshake is likely to refuse plaintext too, and a
   silent downgrade is the behaviour this plan exists to stop. The `tls_failed` count by host is
   the evidence for adding a fallback later, if it is ever needed.
5. **Config:** `probe.starttls: opportunistic | off` (default `opportunistic` after the gate;
   `off` is the escape hatch) and `probe.tls_handshake_timeout` (default 10 s, inside the session
   timeout). Validated at start-up.
6. **What Data Scout sees.** Each result gains `tls: "none" | "verified" | "unverified" | "failed"`
   (additive, `omitempty`); `docs/06-generated/api.md` documents it. Data Scout may ignore it —
   the classes it already maps are unchanged — but it is the field a support answer reads.
7. **Observability:** counter `verify_tls_sessions_total{outcome=none|verified|unverified|failed}`;
   the session log line gains `tls=`.
8. **mxsim learns STARTTLS** (a self-signed cert generated at start, a profile flag
   `starttls: offered|required|broken|absent`) so the session table and the integration suite can
   exercise every path without a real MX.

**Deliberately not here:** DANE / MTA-STS enforcement (that is for *sending* mail with a policy to
honour — plan 014's relay may want it; a verification probe sends no message), and any TLS on the
relay path.

## Tasks

- [ ] `EHLO` capability parse; STARTTLS → `tls.Client` over the vetted conn → re-`EHLO`
- [ ] `VerifyConnection` that records rather than enforces; SNI = MX host; TLS ≥ 1.2
- [ ] `ClassTLSFailed` (`tls_failed`): IsTemp, not IsThrottle, never a verdict; in `docs/06-generated/api.md`
- [ ] Config `probe.starttls`, `probe.tls_handshake_timeout` + validation
- [ ] `tls` field on `Result`; metric `verify_tls_sessions_total`; `tls=` on the session log
- [ ] mxsim: STARTTLS with a generated cert and the four profile modes
- [ ] Tests alongside: advertised → upgraded and re-EHLO'd (the second EHLO's capabilities are the
      ones used); not advertised → plaintext, unchanged; `required` profile answers at RCPT only
      after the upgrade; broken handshake → `tls_failed`, zero `Observe(throttled=true)`, no
      address with `accepted` set; an untrusted / mismatched cert still completes with
      `tls: unverified`; the handshake respects its timeout; SSRF: the TLS layer is given the
      vetted `net.Conn` and the dialler is called exactly once
- [ ] `docs/03-engineering/patterns/` — a short `starttls.md` (policy, why opportunistic, why no fallback)
- [ ] `tech-debt.md` — the STARTTLS item resolved

## Definition of Done

- [ ] **Manual-test gate:** from the node, after deploy, one Data Scout batch that includes the
      ladder's TLS-demanding hosts still answering mail (the 51 are listed in the corpus of plan
      030): **at least 80% of those addresses come back `valid`/`invalid` from the verifier instead
      of `policy`**, Sophos included; `verify_tls_sessions_total{outcome="failed"}` is recorded
      with the hosts behind it; the day's `rcpt_paced` rate per family is unchanged (plan 026's
      gate still holds). Numbers recorded here
- [ ] `go test -race -count=1 ./...` green; vet, gofmt, golangci-lint clean; coverage gate ok
- [ ] `pr-checklist.md` — SSRF (no second dial), IPv4 (unchanged dial), fail-closed (unchanged),
      "us ≠ address" (`tls_failed` is never a verdict) confirmed
- [ ] Docs per Phase 5; `changelog.md`; Status Complete, moved to `completed/`, `ROADMAP.md`

## Notes / decisions / deviations

- **Opportunistic, not mandatory.** Enforcing certificates would turn "this server's cert does not
  name its MX" — the normal case — into "we could not verify the mailbox". Recording the result
  keeps the information without paying for it in coverage.
- **Why a new class rather than `conn_error`.** `conn_error` is `IsThrottle()`; a TLS mismatch at
  one host would halve that host's rate. It must be visible as its own thing and must not move the
  pacer.
- **Open question:** whether to *offer* TLS 1.3 only to Sophos-like filters that ask for it, or
  always (Go's default negotiates 1.3 whenever the server can). The plan assumes always; the gate
  shows whether any host fails the 1.3 handshake that it would have passed at 1.2.
