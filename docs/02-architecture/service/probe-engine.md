# Service — probe engine (`internal/prober`)

One SMTP session per address: connect → `EHLO` → `MAIL FROM` → `RCPT TO` (the address) → `RCPT TO`
(a random local part, for catch-all) → `RSET` → `QUIT`. **`DATA` is never sent.** Ported from
`ds-smtp-retry/ratecheck/internal/prober`.

- Reply → verdict: `docs/03-engineering/patterns/smtp-classification.md` (the only place codes map).
- Catch-all / randomiser detection: the extra bogus `RCPT`; per-domain vs per-server (feature 002).
- Behind an interface so no test opens a socket (mirrors Data Scout's `set_prober` seam).
- Receives only SSRF-vetted IPs from `internal/resolver` (invariant 2).
