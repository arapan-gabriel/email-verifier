# Testing — integration

Against the ported `mxsim` fake MX (from `ds-smtp-retry`) and a real Redis. Scenarios: throttling
knee, greylisting retry, catch-all → risky, policy block → unknown, Redis-down → fail-closed,
concurrent bursts stay under the central bucket.

## Real Redis (plan 027)

The central bucket (invariant 4) is `internal/limiter/token_bucket.lua`, and until plan 027 no test
had executed it. `internal/limiter/redis_integration_test.go` and
`internal/pacer/redis_integration_test.go` run against a real Redis and prove:

- N takes over T seconds admit exactly `burst + rate·T` (the script takes the caller's clock,
  `ARGV[3]`, so the test sets `now` exactly);
- 32 goroutines racing one key never over-admit — the one-round-trip claim — through the script and
  through `pacer.Acquire`;
- burst is honoured and refill is capped at burst;
- `rate`/`burst` ≤ 0 is refused in Go before the script runs;
- Redis disappearing mid-life (a proxy killed under a pooled connection) makes `Take` fail with a
  transport error, never a permissive default (invariant 5);
- a persisted rate and pause survive a new `Pacer` on the same Redis, and a persisted pause is
  honoured **without** taking a token;
- the Go model in `limiter_test.go` agrees with the script, step for step.

**They skip unless `VERIFIERD_TEST_REDIS_ADDR` is set**, and the skip message names the variable
and the `docker run`. CI always sets it against a `redis:7-alpine` service container, and a CI step
fails if any of them skipped. Locally:

```bash
make test-redis        # starts ev-test-redis on 127.0.0.1:56379 if needed, runs the suite with -race
# or by hand
docker run -d --rm --name ev-test-redis -p 127.0.0.1:56379:6379 redis:7-alpine
VERIFIERD_TEST_REDIS_ADDR=127.0.0.1:56379 go test -race -count=1 ./...
```

Every test keys on a random MX name, so pointing it at a shared Redis is safe. This is the one
place local and CI results can differ, and it is deliberate: the alternative is a gate that needs
Docker to run at all.

Two fail-closed tests need no Redis and always run: `TestPacerFailsClosedWithRedisDown` and
`TestRedisDownMeansZeroDials` (the real pacer and limiter over an address nothing listens on — every
address `no_budget`, **zero** connections opened; the plan 003 gate made permanent).
