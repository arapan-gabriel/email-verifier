# Engineering standards — the golden path

Binding for every plan. If a plan deviates, it says so explicitly and records why in its Notes and
in `changelog.md`.

## 1. Language & tooling

- **Go 1.25.** One module, `github.com/arapan-gabriel/email-verifier`.
- Dependencies stay minimal — the lab (`ds-smtp-retry`) is near-zero-dep on purpose. Justify each new
  module in the plan. Sanctioned: `miekg/dns` (resolver), a Redis client (or the lab's minimal RESP
  client), a DKIM library (phase C). No web framework — `net/http` has method-based routing since
  1.22, and a small router is all this service needs.
- Format `gofmt`, lint `golangci-lint`, vet `go vet`, test with `-race`.
- `make gate` runs the Phase-4 gate exactly as CI does. Use it before pushing.

## 2. Layering, wiring, and seams

```
cmd/verifierd  →  internal/api  →  internal/{prober,pacer,resolver,suppress,relay}  →  internal/{limiter,redis,resolver}
```

- **Handlers are thin.** `internal/api` parses, authenticates, calls one engine function, serialises
  a response. No SMTP logic, no Redis calls, no classification in a handler.
- **The engine is transport-agnostic.** `internal/prober` etc. know nothing about HTTP; they take a
  request struct and return a result struct, so the same engine serves HTTP now and a queue worker
  later (ROADMAP 007) with no change.
- No package reaches around a layer. `api` never touches `redis` directly; it goes through the
  engine.

### Interfaces belong to the consumer

Declare an interface in the package that *uses* it, not the package that implements it, and keep it
to the one or two methods actually called. Return concrete types; accept interfaces.

```go
// in internal/api — the only thing the handler needs
type Verifier interface {
    Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}
```

This is what makes the test seams the docs keep demanding real: a fake implementing one method is
trivial, so no test opens a socket (mirroring `smtp_probe.py`'s `set_prober`). A fat interface
exported by the implementer produces the opposite — every test needs the whole engine.

`api.ReadinessFunc` is the pattern in its smallest form: a named function type the router owns,
satisfied by `RedisReachable` in production and by a closure in tests.

### Dependencies are passed in, never reached for

- No package-level mutable state and no `init()`. Beyond the usual reasons, ADR-004 forbids pacing
  state a second node cannot see — a package-level variable is exactly that, and the rule is easier
  to keep when nothing in the codebase does it.
- Constructors take an explicit `Options` struct. Prefer it to variadic functional options: the set
  of knobs here is small, and a struct is greppable, zero-value-sane, and readable at the call site.
- `main` is a five-line wrapper around `run(ctx, args, getenv, stderr) error`. Every ambient
  dependency — arguments, environment, error stream, cancellation — is a parameter, so startup,
  shutdown and configuration failures are testable without spawning a process. `cmd/verifierd` is
  the reference implementation.

## 3. Configuration

- One source: `internal/config`, loaded from env (+ optional YAML). Validated at startup; the
  service refuses to boot on a bad/missing required value. No hardcoded IPs, hostnames, secrets, or
  Redis URLs anywhere else (invariant 10).
- `Load` takes a `getenv func(string) string` rather than reading the process environment, so tests
  exercise real precedence without `t.Setenv` (which serialises tests and leaks across helpers).
- Validation reports **every** problem via `errors.Join`, not the first one. An operator fixing a
  unit file should not need one restart per mistake.

## 4. Errors

Error handling is not boilerplate in this service — it *is* the correctness model. Invariant 1 says
a rejection of us is never a verdict about the address, and that distinction is carried entirely by
how failures are typed.

- **Every error is classifiable by type, never by message text.** Sentinel values
  (`var ErrInvalid = errors.New(...)`) or typed errors, tested with `errors.Is` / `errors.As`.
  `strings.Contains(err.Error(), "5.1.1")` is forbidden: it silently rots when a provider rewords a
  reply, and the failure mode is deleting a live mailbox.
- Wrap with `%w` and add context at each layer; never both log and return the same error.
- The mapping from an SMTP reply to a verdict lives in exactly one place (`internal/prober`
  classifier) and follows `docs/03-engineering/patterns/smtp-classification.md`.
- A verdict is one of `valid | invalid | risky | unknown`. **When in doubt, `unknown`** (invariant 1).
- HTTP errors use one JSON shape: `{error: {code, message}}`. Transport errors (auth, bad request)
  are never confused with verification verdicts — a malformed request is a `400`, not an `unknown`.

## 5. Concurrency & safety

- **`context.Context` is the first parameter** of every function that performs IO or can block, and
  it is honoured: a cancelled request must abandon its socket, not leak a goroutine holding one.
  Never store a context in a struct.
- All outbound network work is context-bound with a timeout. No unbounded goroutines; the probe
  budget is enforced by the central bucket (`internal/limiter`), never by ad-hoc semaphores that a
  second node cannot see.
- Dial `tcp4`, never `tcp` (invariant 3). The systemd unit's `RestrictAddressFamilies` enforces the
  same rule at the kernel level; code and deployment back each other up.
- Every resolved MX IP passes the SSRF guard before a socket opens (invariant 2).
- Rate ceilings **fail closed** (invariant 5): Redis error → skip, return `unknown`.

## 6. State

- No SQL database. Operational state only, in Redis, under the `ds-smtp-retry` key contract
  (`docs/06-generated/redis-contract.md`). Business verdicts belong to Data Scout.
- Any Redis key added is documented in `redis-contract.md` in the same change.

## 7. Testing

- **Table-driven by default**, especially the classifier and pacer. See `docs/03-engineering/testing/`.
- The SMTP transport and Redis sit behind consumer-side interfaces (§2), so unit tests open no
  sockets. Integration tests use the ported `internal/mxsim` fake MX and a local Redis.
- **Anything time-dependent is tested with `testing/synctest`** (GA in Go 1.25). Inside a bubble the
  `time` package runs on a fake clock that only advances when every goroutine is durably blocked, so
  a five-minute MX cooldown, a thirty-minute greylist retry, or a token-bucket refill window is
  asserted in microseconds and deterministically.

  ```go
  synctest.Test(t, func(t *testing.T) {
      resumed := make(chan struct{})
      go func() { time.Sleep(5 * time.Minute); close(resumed) }()
      synctest.Wait()          // every goroutine is durably blocked
      // ... assert still paused, advance, assert resumed
  })
  ```

  **Do not port the lab's hand-rolled `Clock` interface into the service.** `ds-smtp-retry` predates
  `synctest` and threads an injectable clock through its call graph; Go 1.25 fakes `time` itself, so
  the service uses `time` directly and keeps the production signatures clean. (`internal/mxsim`
  keeps its own clock — it is ported code, deliberately left close to upstream.)
- Use `t.Context()` rather than `context.Background()` in tests; it is cancelled at test end, so a
  leaked goroutine fails loudly instead of hanging the suite.
- `-race` is part of the gate, not an optional pass. Concurrent verification is the whole workload;
  a data race here corrupts pacing state shared across every MX.
- Mandatory regression tests for every invariant: us≠address, SSRF refusal, fail-closed,
  policy≠throttle, IPv4-only.

## 8. Security posture

- The HTTP surface is internet-facing: authenticated (mTLS/API key) except health probes
  (invariant 11). Authenticated routes are registered through one guard helper so a route cannot
  silently skip it.
- Credential comparison is constant-time (`crypto/subtle`); a plain `==` leaks the key one byte at a
  time through response timing.
- Suppression is honoured before any probe or send (invariant 9).
- The service is designed to be *safe to point at strangers' MXes*: paced, guarded, fail-closed. A
  bug that floods someone's MX is the highest-severity class of defect here.
