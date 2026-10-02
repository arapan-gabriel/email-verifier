# Plan 031 — STARTTLS on the probe

**Status:** Code complete 2026-10-02 (plaintext fallback added the same day after a regression on the node — see Notes) — redeploy and the manual-test gate pending
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
   unattempted with a retry hint (invariant 1). ~~No plaintext fallback in this plan~~ —
   **superseded 2026-10-02 (regression, see Notes):** a refused `STARTTLS` carries on in plaintext
   on the same connection (`tls: fallback`, nothing remembered); a failed handshake closes, redials
   **once** in plaintext (`tls: fallback`) and remembers the host for 24 h
   (`mx:<host>:tls_broken`, later sessions `tls: skipped`). `tls_failed` remains only for a
   handshake failure whose plaintext retry also fails.
5. **Config:** `probe.starttls: opportunistic | off` (default `opportunistic` after the gate;
   `off` is the escape hatch) and `probe.tls_handshake_timeout` (default 10 s, inside the session
   timeout). Validated at start-up.
6. **What Data Scout sees.** Each result gains `tls: "none" | "verified" | "unverified" | "fallback" | "skipped" | "failed"`
   (additive, `omitempty`); `docs/06-generated/api.md` documents it. Data Scout may ignore it —
   the classes it already maps are unchanged — but it is the field a support answer reads.
7. **Observability:** counter `verify_tls_sessions_total{outcome=none|verified|unverified|fallback|skipped|failed}`;
   the session log line gains `tls=`.
8. **mxsim learns STARTTLS** (a self-signed cert generated at start, a profile flag
   `starttls: offered|required|broken|absent`) so the session table and the integration suite can
   exercise every path without a real MX.

**Deliberately not here:** DANE / MTA-STS enforcement (that is for *sending* mail with a policy to
honour — plan 014's relay may want it; a verification probe sends no message), and any TLS on the
relay path.

## Tasks

- [x] `EHLO` capability parse; STARTTLS → `tls.Client` over the vetted conn → re-`EHLO` — `advertisesSTARTTLS`, `Prober.upgrade`; the second EHLO's capabilities are the ones used
- [x] `VerifyConnection` that records rather than enforces; SNI = MX host; TLS ≥ 1.2 — `tls: verified | unverified`; TLS 1.3 offered
- [x] `ClassTLSFailed` (`tls_failed`): IsTemp, not IsThrottle, never a verdict; in `docs/06-generated/api.md` — also gets a retry hint (`retryHint`)
- [x] Config `probe.starttls`, `probe.tls_handshake_timeout` + validation — env `PROBE_STARTTLS`, `PROBE_TLS_HANDSHAKE_TIMEOUT`; the timeout must be below `probe.timeout`
- [x] `tls` field on `Result`; metric `verify_tls_sessions_total`; `tls=` on the session log — the log field rides the `smtp_reply` line (`tls`), there being no other per-session line
- [x] mxsim: STARTTLS with a generated cert and the four profile modes — `starttls: absent|offered|required|broken`, one self-signed ECDSA cert per process, `smtp.Certificate()` for tests
- [x] Tests alongside: advertised → upgraded and re-EHLO'd (the second EHLO's capabilities are the — `starttls_integration_test.go` (mxsim: offered/absent/required/broken, trusted cert → verified, one dial) and `starttls_test.go` (capability parse, a declined STARTTLS, the handshake timeout, `off`, no TLS field without a session)
      ones used); not advertised → plaintext, unchanged; `required` profile answers at RCPT only
      after the upgrade; broken handshake → `tls_failed`, zero `Observe(throttled=true)`, no
      address with `accepted` set; an untrusted / mismatched cert still completes with
      `tls: unverified`; the handshake respects its timeout; SSRF: the TLS layer is given the
      vetted `net.Conn` and the dialler is called exactly once
- [x] `docs/03-engineering/patterns/` — a short `starttls.md` (policy, why opportunistic, why no fallback)
- [x] `tech-debt.md` — the STARTTLS item resolved
- [x] **Regression fix (2026-10-02): plaintext fallback like an MTA at `may`** — refused STARTTLS →
      same-session plaintext; failed handshake → lease released, one plaintext redial (SSRF guard and
      `tcp4` unchanged, tokens and `OnPaced` per RCPT as any session), host remembered in
      `mx:<host>:tls_broken` (profile TTL, fail-open read); `tls_failed` only when the retry fails
      too; `tls: fallback | skipped`; metric outcomes `fallback`, `skipped`. Tests: declined →
      same-session verdict, 1 token, not remembered; broken handshake (mxsim) → verdicts, 2 dials,
      one `fallback` metric; remembered host → no STARTTLS sent, 1 dial, `skipped`; memory down →
      TLS tried, probe answered; handshake + retry fail → `tls_failed`, `failed` metric, host
      remembered; one-slot lease → released before the rerun, `OnPaced` = tokens; mxprofile key,
      TTL, per-host, fail-open

## Definition of Done

- [ ] **Manual-test gate:** from the node, after deploy, one Data Scout batch that includes the
      ladder's TLS-demanding hosts still answering mail (the 51 are listed in the corpus of plan
      030): **at least 80% of those addresses come back `valid`/`invalid` from the verifier instead
      of `policy`**, Sophos included; `verify_tls_sessions_total{outcome="failed"}` is recorded
      with the hosts behind it; the day's `rcpt_paced` rate per family is unchanged (plan 026's
      gate still holds). Numbers recorded here
- [x] `go test -race -count=1 ./...` green; vet, gofmt, golangci-lint clean; coverage gate ok — 2026-10-02 with `VERIFIERD_TEST_REDIS_ADDR`; golangci-lint 0 issues; prober 96.3% (floor 92), total 93.2%. Re-run after the fallback fix, same day: all green, prober 94.8%, total 92.8%
- [x] `pr-checklist.md` — SSRF (no second dial), IPv4 (unchanged dial), fail-closed (unchanged),
      "us ≠ address" (`tls_failed` is never a verdict) confirmed. After the fix: the fallback's one
      redial goes through the same guarded dialler over `tcp4`; the verdicts kept come only from the
      plaintext session's own RCPT replies
- [ ] Docs per Phase 5; `changelog.md`; Status Complete, moved to `completed/`, `ROADMAP.md`

## Notes / decisions / deviations

- **Opportunistic, not mandatory.** Enforcing certificates would turn "this server's cert does not
  name its MX" — the normal case — into "we could not verify the mailbox". Recording the result
  keeps the information without paying for it in coverage.
- **Why a new class rather than `conn_error`.** `conn_error` is `IsThrottle()`; a TLS mismatch at
  one host would halve that host's rate. It must be visible as its own thing and must not move the
  pacer.
- ~~**Open question:** whether to offer TLS 1.3 only to Sophos-like filters, or always.~~ **Decided
  2026-10-02: always** — TLS 1.3 and 1.2 offered, 1.2 the floor, Go's default negotiation. Sophos's
  refusal was "TLS version is not available" — we offered none — and a modern stack is what such
  filters expect. The gate's `failed` count shows any host that a 1.3-first handshake loses.
- **Implementation notes (2026-10-02).** The TLS outcome rides the existing `smtp_reply` journal
  line (`tls`) rather than a new per-session line; the counter covers every session. An old prober
  test whose scripted server listed `STARTTLS` and answered `250 ok` to it now (rightly) produced
  `tls_failed`; its script lists `8BITMIME` instead, keeping the multi-line EHLO it was about.
  Mutation check: with the STARTTLS step disabled, 6 tests fail.

### Regression on the node, and the fallback (2026-10-02)

- **What happened.** Deployed 11:00 UTC (`a2037af`). In the 4-job run that followed, 33 sessions
  were `tls_failed`; **32 were Jimdo's `mx1.jimdo.com`** (`tls handshake: remote error: tls:
  handshake failure`), 1 `mx1.emailsrvr.com`. Those addresses had been plaintext verdicts the day
  before; under this plan they became `unknown` + recheck — the opposite of the plan's goal.
- **What Jimdo supports** (`openssl s_client -starttls smtp -connect mx1.jimdo.com:25` from the
  node): TLS 1.3 → `alert handshake failure`; TLS 1.1/1.0 → no protocols; **TLS 1.2 only, with
  `DHE-RSA-AES256-GCM-SHA384`** — finite-field Diffie-Hellman, which Go's `crypto/tls` does not
  implement. No configuration of our client can complete that handshake; OpenSSL-based MTAs can,
  and an MTA that cannot (or whose handshake fails) delivers in plaintext. (`jimdo.com`'s own MX is
  Google; `mx1.jimdo.com` serves Jimdo-hosted customer domains.)
- **The decision in design §4 was wrong for opportunistic TLS.** "A server that failed the handshake
  is likely to refuse plaintext too" was not what the node showed; Jimdo answers plaintext fine.
  The fallback is the behaviour of every `may`-level MTA, and the probe sends no message, so a
  downgrade exposes only the RCPT questions the pre-031 probe already asked in plaintext.
- **Design decisions.**
  - *Refused `STARTTLS`* (non-`220`, e.g. `454 4.7.0`): continue on the same connection in
    plaintext (RFC 3207 §4 allows it; the connection is still in its plaintext state). No redial, no
    extra token, **not remembered** — a `454` is usually a transient "not now".
  - *Failed handshake / handshake timeout / connection lost after `220`*: the connection is unusable,
    so close it, **release the 028 lease first** (idempotent release, else a one-slot pace key would
    wait for itself), and run the session again **once** with TLS disabled — a fresh dial through the
    same SSRF guard and `tcp4`, drawing its own tokens (`OnPaced` per token, so the failed attempt's
    token is counted too). The rerun is flagged so it can never try STARTTLS or recurse.
  - *Memory*: `mx:<host>:tls_broken` = `1` with the profile TTL (`probe.randomiser_ttl`, 24 h), per
    MX host (never per pace key). Later sessions skip STARTTLS (`tls: skipped`), saving the handshake
    and the extra token. Read fails open: Redis down → try TLS, fall back if needed; the write is
    best effort.
  - *`tls_failed`* only when the plaintext retry also cannot reach the server (`err` names both
    failures, retry hint kept, `tls: failed`, metric `failed`). A retry that connects reports its
    own classes (a timeout mid-session stays `timeout`).
  - *Metric*: each session counted once — `fallback` for a retry that answered, `failed` only when
    it did not; the abandoned attempt is not counted, so `fallback + skipped` is the "would have been
    lost" number.
- **Mutation check** (each mutant must fail at least one test): rerun removed → 4 tests fail; host
  not remembered → 1; lease not released before the rerun → 1; memory ignored → 1; a `454`
  remembered → 1; a `454` treated as a handshake failure → 1; `failed` metric not recorded → 1;
  retry failure not re-classed `tls_failed` → 1. All 8 killed.
