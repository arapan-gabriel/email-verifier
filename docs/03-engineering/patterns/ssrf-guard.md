# Pattern — SSRF guard on resolved MX

**New code for this service** — the one place `ds-smtp-retry`'s lab does the opposite on purpose
(the lab connects to `127.0.0.1` MXes by design). Mirrors Data Scout's plan-024 guard.

## Why

The MX host is attacker-influenced data: a domain owner can point their MX record at `127.0.0.1`,
`169.254.169.254` (cloud metadata), or a private range, and a naive prober would then open a socket
from inside our network to an internal target. That is a server-side request forgery.

## Rule (invariant 2)

Every resolved IP — after MX→A resolution, before any socket — is rejected if it is:

- loopback (`127.0.0.0/8`, `::1`),
- private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`),
- link-local (`169.254/16`, `fe80::/10`) — includes cloud metadata,
- unspecified / multicast / reserved.

A domain whose MX resolves only to such addresses is **not probed**; the address returns `unknown`
with a reason, never `invalid` (it is our refusal, not the mailbox's absence).

## Where

`internal/resolver` performs resolution and applies the guard, so no caller can bypass it — the
prober only ever receives already-vetted, routable IPs. Tested with a domain (or stub resolver)
whose MX points at `127.0.0.1`: the probe must be refused, not attempted.
