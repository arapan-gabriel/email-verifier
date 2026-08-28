# ADR-005 — Deploy as a systemd service, not a container

**Status:** Accepted (2026-08-26)

## Context

This service runs on a single-purpose VPS whose entire reason for existing is the identity of its
egress IP. The workload on that host is two processes: one static Go binary and a Redis holding
operational state.

Plans 000 and 013 originally specified both forms without choosing — a distroless image with
`docker-compose` (verifierd + Redis), *or* a systemd unit. Containers are the default reflex for a
Go service, so leaving the "or" unresolved meant the choice would be made by habit at deploy time
rather than on the merits. This ADR settles it.

## Decision

Deploy `verifierd` as a **static binary under `systemd`**, with Redis from the distribution
package. **No container runtime on the isolated-IP host.**

The release artifact is the binary from `CGO_ENABLED=0 go build`; the repo ships
`packaging/verifierd.service` and installs it verbatim.

## Why

1. **The egress IP is the product, and bridge NAT hides it.** `source_ip` is returned with every
   verdict and is the basis of the Data Scout contract (ADR-003: a verdict is only as good as the IP
   that produced it). Under Docker's default bridge the process observes `172.17.x.x`, so the real
   address has to be supplied by configuration — the one value in this system that must never be
   guessed becomes a guess, and one that can silently drift from reality after any host change.
2. **`172.16/12` is precisely what the SSRF guard refuses** (invariant 2). Running the prober inside
   the range it is programmed to reject puts an avoidable ambiguity on the safety-critical path —
   the path where a wrong call is either an SSRF hole or a list-destroyer.
3. **The token bucket is on the hot path.** Take+refill runs once per probe (invariant 4).
   Substituting a bridge hop for a unix-socket hop is strictly slower and adds a failure mode to a
   path that is required to fail closed (invariant 5). Fewer moving parts there is a correctness
   argument, not a performance one.
4. **There is nothing to isolate.** A `CGO_ENABLED=0` Go binary has no runtime dependencies; that
   binary already *is* the immutable, reproducible artifact an image would wrap. Wrapping it adds a
   build step, a registry, and a daemon in exchange for nothing.
5. **The hardening is available without the runtime.** `ProtectSystem=strict`, `NoNewPrivileges`,
   `PrivateTmp`, an empty `CapabilityBoundingSet`, `SystemCallFilter=@system-service` and friends
   cover the isolation a container was nominally providing — worth having on an internet-facing
   edge, and obtainable from the init system already present.
6. **Debuggability.** This service's steady-state question is "why did this SMTP or DNS connection
   behave this way, against this MX, from this IP". Every layer between the process and the wire is
   another layer to reason through, and that diagnosis is the daily work here, not an edge case.

## Consequences

- Plans **000** and **013** carry systemd: 000 ships `packaging/verifierd.service` and a `Makefile`
  holding the static-build flags; 013 installs the binary, the unit, a `verifierd` system user, and
  a distro Redis. 013's Definition of Done asserts that the returned `source_ip` equals the host's
  real egress address — the check that consequence 1 above exists to protect.
- CI publishes the binary as an artifact. Deploy is copy + `systemctl restart`; rollback is keeping
  the previous binary alongside. `TimeoutStopSec=30s` gives the graceful shutdown time to drain
  in-flight SMTP sessions.
- Redis is never network-exposed: unix socket preferred, loopback as the fallback. The RESP client
  ported from the lab is TCP-only today — tracked in `tech-debt.md`.
- Local development uses a locally installed Redis. The repo prescribes no way to start it and
  maintains no compose file.
- Host-level concerns become genuinely host-level and must be written down in plan 013 rather than
  assumed away by an image: the service user, the firewall, the absence of any inbound MTA, and the
  sender identity (rDNS/FCrDNS/SPF/DKIM/DMARC).
- **ADR-004 is unaffected.** A static binary is identical by construction across nodes, so "add node
  #2" remains a deployment step and never an engine change. This ADR governs the host, not the
  build: if a registry is ever wanted for fleet rollout, it can be added then.

## Alternatives rejected

- **Distroless image + `docker-compose` (verifierd + Redis)** — the original plan-000 shape.
  Rejected on reasons 1–3: it obscures the one address the whole architecture is about, places the
  prober inside an IP range the prober is built to refuse, and lengthens the pacing hot path — all
  to isolate a binary that has nothing to isolate.
- **Container with `network_mode: host`** — removes the NAT and hot-path objections, but what
  remains is namespace isolation that `systemd` already provides, while keeping the runtime, the
  image build, and the registry as pure overhead.
- **Nix or a distro package for the service itself** — real reproducibility gains in general, but no
  gain over a static binary here, and a new toolchain to operate on an edge box in exchange.
