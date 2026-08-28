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

Beyond the classics above, the implementation also refuses carrier-grade NAT (`100.64/10`),
benchmarking (`198.18/15`), the documentation blocks, IETF assignments (`192.0.0/24`), and the IPv6
tunnelling prefixes — 6to4 (`2002::/16`), Teredo (`2001::/32`), NAT64 (`64:ff9b::/96`) and
v4-mapped (`::ffff:0:0/96`) — because each can embed an arbitrary IPv4 address and carry it inward.

The rule is expressed as **deny by default**: anything that is not routable global unicast is
refused, and the named ranges cover what the standard-library predicates miss. Over-blocking costs a
rare unnecessary "not attempted"; under-blocking is a hole.

## Where

`internal/resolver` resolves and vets in one step, and `internal/prober` receives `netip.Addr`
values rather than a hostname — so the guard sits between the lookup and the socket, where it has to
be. Handing a *name* to a dialer puts the resolution on the wrong side of the guard and is the exact
bug plan 002 closed.

Two details that matter:

- **An IP literal is vetted directly, without a lookup.** Otherwise `mx_host: "127.0.0.1"` depends
  on what the resolver chooses to do with a literal.
- **The guard is the prober's default, not an option.** Forgetting to wire it cannot produce an
  unguarded prober.

A refusal is `class: guarded` with a reason: `connected:false`, `accepted:null`, never `invalid`.
It is neither a throttle nor a deferral — retrying changes nothing, and slowing down does not change
someone's DNS record.
