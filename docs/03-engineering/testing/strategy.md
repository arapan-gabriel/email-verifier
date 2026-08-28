# Testing — strategy

- **Unit** (default): table-driven, no sockets. The SMTP transport and Redis are behind interfaces
  so tests are deterministic (mirror Data Scout's `set_prober` seam).
- **Integration**: the engine against a fake SMTP server (port the `ds-smtp-retry` `mxsim`
  simulator: 421 throttling, greylisting, catch-all, tarpits, injectable clock) and a real/local
  Redis. Assert on what the *server* saw, not just the client's verdict.
- **Time-dependent behaviour uses `testing/synctest`** (Go 1.25). Cooldowns, greylist backoff,
  bucket refill windows and AIMD recovery are asserted on a fake clock in microseconds, not slept
  through. Do not port the lab's hand-rolled `Clock` into the service — see
  `ENGINEERING-STANDARDS.md` §7.
- The gate is `go test -race -count=1 ./...`, matching CI exactly.
- Mandatory regression tests for every invariant: us≠address, SSRF refusal, fail-closed, policy≠throttle.
