# Engineering standards — the golden path

Binding for every plan. If a plan deviates, it says so explicitly and records why in its Notes and
in `changelog.md`.

## 1. Language & tooling

- **Go 1.25.** One module, `github.com/<org>/email-verifier`.
- Dependencies stay minimal — the lab (`ds-smtp-retry`) is near-zero-dep on purpose. Justify each new
  module in the plan. Sanctioned: `miekg/dns` (resolver), a Redis client (or the lab's minimal RESP
  client), a DKIM library (phase C). No web framework — `net/http` + a small router.
- Format `gofmt`, lint `golangci-lint`, vet `go vet`, test with `-race`.

## 2. Layering

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

## 3. Configuration

- One source: `internal/config`, loaded from env (+ optional YAML). Validated at startup; the
  service refuses to boot on a bad/missing required value. No hardcoded IPs, hostnames, secrets, or
  Redis URLs anywhere else (invariant 10).

## 4. Errors & verdicts

- A verdict is one of `valid | invalid | risky | unknown`. The mapping from an SMTP reply to a
  verdict lives in exactly one place (`internal/prober` classifier) and follows
  `docs/03-engineering/patterns/smtp-classification.md`.
- **Never turn a rejection of us into `invalid`** (invariant 1). When in doubt, `unknown`.
- HTTP errors use one JSON shape: `{error: {code, message}}`. Transport errors (auth, bad request)
  are never confused with verification verdicts — a malformed request is a `400`, not an `unknown`.

## 5. Concurrency & safety

- All outbound network work is context-bound with a timeout. No unbounded goroutines; the probe
  budget is enforced by the central bucket (`internal/limiter`), never by ad-hoc semaphores that a
  second node cannot see.
- Every resolved MX IP passes the SSRF guard before a socket opens (invariant 2).
- Rate ceilings **fail closed** (invariant 5): Redis error → skip, return `unknown`.

## 6. State

- No SQL database. Operational state only, in Redis, under the `ds-smtp-retry` key contract
  (`docs/06-generated/redis-contract.md`). Business verdicts belong to Data Scout.
- Any Redis key added is documented in `redis-contract.md` in the same change.

## 7. Testing

- Table-driven unit tests for the classifier and pacer; the SMTP layer is behind an interface so no
  test opens a real socket (mirror `smtp_probe.py`'s `set_prober` seam). See
  `docs/03-engineering/testing/`.
- `go test -race -count=1 ./...` is the gate; it must match CI exactly.

## 8. Security posture

- The HTTP surface is internet-facing: authenticated (mTLS/API key) except health probes.
- Suppression is honoured before any probe or send (invariant 9).
- The service is designed to be *safe to point at strangers' MXes*: paced, guarded, fail-closed. A
  bug that floods someone's MX is the highest-severity class of defect here.
