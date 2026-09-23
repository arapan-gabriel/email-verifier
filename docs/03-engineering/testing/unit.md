# Testing — unit

Table-driven. Classifier: `(code,text)→Class`. Pacer: sequences of samples → rate/conc/state
transitions, including "deferral does not move the pacer" and "throttle does". No real sockets or
Redis — both behind consumer-side interfaces (`ENGINEERING-STANDARDS.md` §2).

Anything involving elapsed time runs inside `synctest.Test`: a pauser that must hold an MX for five
minutes is verified without the suite taking five minutes, and without the flakiness a real clock
introduces under `-race`. `net.Pipe` works inside a bubble, so a tarpitting SMTP server is tested
there too (`TestTarpitBannerTimesOut`: the 20-second session timeout elapses instantly).

**The SMTP state machine is one table** (`internal/prober/session_table_test.go`, plan 027). Each
row is a server transcript (`transcriptMX`: replies by command prefix, "close on the Nth X",
silence) plus fakes, and asserts per-address classes, `Observe(throttled)` calls, tokens taken, IP
health's policy count and what `record()` reported. Every row also asserts invariant 1 directly:
no result may be `invalid` without an `RCPT` answer. Adding a branch to `session` means adding a
row.

**The HTTP edge is one route table** (`internal/api/routes_test.go`). `everyRoute` lists every
route `NewRouter` can register, kept in step with `docs/06-generated/api.md`; the test builds the
router with every engine present and proves each non-health route answers `401` +
`WWW-Authenticate` to no header, a wrong token and malformed schemes (invariant 11). **Adding a
route means adding a row.** The mTLS edge (`clientAuthTLS`) is tested through real handshakes with
certificates generated in-test — no fixtures on disk.

Fakes must not drift from what they stand in for. `internal/limiter`'s `fakeStore` is a faithful Go
model of `token_bucket.lua` (per-key tokens and timestamp, refill capped at burst, the caller's
clock), and `TestFakeAgreesWithRealScript` requires the two to agree step for step against a real
Redis (see `integration.md`).
