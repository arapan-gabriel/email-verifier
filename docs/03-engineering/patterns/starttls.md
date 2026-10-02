# Pattern — opportunistic STARTTLS on the probe (plan 031)

Many receiving servers refuse a plaintext client — `530 5.7.0 Must issue a STARTTLS command first`,
Sophos's `5.7.4 … TLS version is not available` — and before plan 031 the prober had no STARTTLS
step, so every such server answered *us* (`policy`) and never the mailbox. 110 rows over 51 hosts on
the warm-up ladder.

## The sequence

`connect → banner → EHLO → [STARTTLS → 220 → TLS handshake → EHLO] → MAIL FROM → RCPT …`

- The bracket runs only when the first EHLO lists `STARTTLS` and `probe.starttls` is
  `opportunistic` (the default). `off` is the escape hatch: plaintext always.
- The handshake runs **over the connection already dialled to a vetted address**. No second dial,
  no hostname resolution — the SSRF guard (invariant 2) and `tcp4` (invariant 3) are untouched.
- EHLO is **repeated over TLS**: RFC 3207 §4.2 discards everything learned before the upgrade.
- The session lease (plan 028), the pacer tokens and plan 029's catch-all sequence are all inside the
  same session, so the upgrade costs no budget and changes nothing about pacing.

## The policy: an opportunistic MTA's

- SNI = the MX name; TLS 1.2 is the floor and 1.3 is offered (Go's default negotiation).
- **The certificate is checked for the record, never enforced** (`InsecureSkipVerify` plus a
  `VerifyConnection` that runs the normal chain-and-name check and records the result). Most MX
  certificates do not name the MX; a sender that refused them would refuse half the internet.
  Postfix's `smtp_tls_security_level = may` is the same choice. The outcome is reported as
  `tls: verified | unverified`.
- No client certificate.

## When it fails — fall back, like an MTA at `may`

The first version (2026-10-02 morning) ended any failed upgrade as `tls_failed` with no plaintext
retry. On the day it shipped, 32 of 33 `tls_failed` sessions were Jimdo's `mx1.jimdo.com`, which
offers STARTTLS but speaks only TLS 1.2 with `DHE-RSA-AES256-GCM-SHA384` — a finite-field DHE suite
Go's `crypto/tls` does not implement, so the handshake can never succeed from this client. Those were
verdicts the day before. An opportunistic MTA (Postfix `smtp_tls_security_level = may`) delivers to
such a server in plaintext; the probe now does the same:

| What happened | What the prober does | `tls` | Remembered? |
|---|---|---|---|
| `STARTTLS` answered non-`220` (e.g. `454 4.7.0`) | carries on **in the same connection** in plaintext (RFC 3207 §4: the client may continue without TLS) | `fallback` | no — a `454` is "not now"; the next session asks again |
| `220`, then the handshake fails, times out (`probe.tls_handshake_timeout`, 10 s), or the connection drops | closes, **once** redials the same vetted address set in plaintext and asks everything again | `fallback` | yes — `mx:<host>:tls_broken`, profile TTL (24 h) |
| host remembered as TLS-broken | plaintext without sending `STARTTLS` | `skipped` | already |
| the plaintext retry cannot connect either | class **`tls_failed`**: no verdict, `connected:false`, a retry hint, `err` names both failures | `failed` | yes |

Rules the retry keeps:

- **One retry, never a loop**: the rerun is marked and never tries STARTTLS.
- **The lease is given back first** (plan 028): the failed session releases its lease before the
  rerun takes its own, so a one-session pace key never waits for itself.
- **Every token counts**: the rerun draws its tokens like any session, and each one fires `OnPaced`,
  so the warm-up's `rcpt_paced` total includes the failed attempt's token.
- **The redial is an ordinary dial**: through the SSRF guard (invariant 2) and over `tcp4`
  (invariant 3), to the addresses the session already vetted.
- **Memory fails open**: if Redis is down, `TLSBroken` reads false and the session tries TLS (and
  falls back if it must); the failed write is ignored.
- Still never a throttle (invariant 6) and never a verdict from the failed half (invariant 1).
- `verify_tls_sessions_total` counts each session once: `fallback` for the retry that answered,
  `failed` only when it did not; the abandoned TLS attempt is not counted.

A silent downgrade is what an active attacker could force — but the probe sends no message and no
secret, only the RCPT questions a plaintext probe asked before 031; the downgrade's cost is nil, and
refusing it cost real answers.

## Not here

DANE / MTA-STS enforcement is for *sending* a message under a published policy (plan 014's relay);
a verification probe sends no message.
