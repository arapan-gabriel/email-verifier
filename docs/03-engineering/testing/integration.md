# Testing — integration

Against the ported `mxsim` fake MX (from `ds-smtp-retry`) and a local Redis. Scenarios: throttling
knee, greylisting retry, catch-all → risky, policy block → unknown, Redis-down → fail-closed,
concurrent bursts stay under the central bucket.
