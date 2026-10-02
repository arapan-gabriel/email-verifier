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

## When it fails

A non-`220` answer to `STARTTLS`, or a failed or timed-out handshake (`probe.tls_handshake_timeout`,
default 10 s, inside `probe.timeout`), is class **`tls_failed`**:

- never a verdict (invariant 1) — `accepted: null`, `connected: false`;
- never a throttle (invariant 6) — `conn_error` would have halved the host's rate for what is a
  TLS mismatch, not our pace;
- a retry hint like any "come back later";
- **no plaintext retry in that session.** A server that offered TLS and then failed it is likely to
  refuse plaintext too, and a silent downgrade is the behaviour this plan exists to stop. The
  `verify_tls_sessions_total{outcome="failed"}` count is the evidence a fallback would need.

## Not here

DANE / MTA-STS enforcement is for *sending* a message under a published policy (plan 014's relay);
a verification probe sends no message.
