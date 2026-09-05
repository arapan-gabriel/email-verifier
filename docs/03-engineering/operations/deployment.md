# Operations — deployment

As built by plan 013 on `92.222.87.97` (OVH VPS-1, France, Debian 13, `datascoutmail.com`).
One static binary under `systemd` beside a distro Redis. **No container runtime** (ADR-005) — the
sandboxing directives in `packaging/verifierd.service` are what a container would have provided.

## Why the host looks the way it does

- A **full region**, never an OVH Local Zone: a VPS there cannot reach any SMTP port and cannot be
  unblocked on request, so the project would be dead on arrival with no diagnosis path.
- The `/24` is checked for listing **before** anything is built on the host. Recycled VPS addresses
  arrive pre-listed, and destroying the VPS for a fresh address is far cheaper than delisting.
- **No inbound MTA.** Debian images may ship `exim4`; a stray listener on `:25` is an open-relay
  risk that burns the IP faster than any probing mistake. `ss -tlnp | grep ':25'` must be empty.
- **IPv4 only.** The published identity (PTR, FCrDNS, SPF) covers the IPv4 address alone. The host
  is dual-stack and will otherwise leave over IPv6 — invariant 3, and the trap that made the
  preflight itself report NO-GO on a healthy node (see below).

## Layout

| Path | Owner / mode | What |
|---|---|---|
| `/usr/local/bin/verifierd` | `root:root 755` | the static binary (`CGO_ENABLED=0`) |
| `/etc/systemd/system/verifierd.service` | `root:root 644` | `packaging/verifierd.service`, verbatim |
| `/etc/verifierd/verifierd.yaml` | `root:verifierd 640` | `config/verifierd.yaml` with the host's `http.addr` and `tls.*` |
| `/etc/verifierd/env` | `root:verifierd 640` | `VERIFIERD_AUTH_API_KEY` — the only secret, never in the unit or the repo (invariant 10) |
| `/etc/verifierd/tls/` | `root:verifierd 750` | CA, server pair, `ca.pem` as the client CA |
| `/etc/verifierd/tls/client/` | `root:root 700` | the client bundle Data Scout is issued (plan 008) |
| `/etc/verifierd/dkim/s1.private` | `verifierd:verifierd 600` | DKIM key, phase C |
| `/usr/local/lib/verifierd/preflight.sh` | `root:root 755` | `scripts/preflight.sh` |
| `/usr/local/bin/verifierd-preflight` | `root:root 755` | wrapper supplying the domain, HELO name and DKIM selector |

Redis is the distro package with `port 0`, `unixsocket /run/redis/redis-server.sock`,
`unixsocketperm 660`, `appendonly yes`, `appendfsync everysec`. `verifierd` is in the `redis` group.
AOF is not optional: stock Debian ships RDB snapshots only, and the pacing state is what a restart
must not lose. The RESP client dials the socket directly (`redis.New` splits a `unix:` prefix), so
there is no loopback TCP hop on the token-bucket hot path.

## The start gate

`ExecStartPre=/usr/local/bin/verifierd-preflight` is a **hard** precondition, not advisory. A deploy
onto a blocked or reverse-DNS-broken IP produces verdicts that measure nothing (RUNBOOK). The script
exits non-zero only on a `[FAIL]`, so an advisory — the ASN-wide UCEPROTECT L3 listing, a DNSBL zone
refusing queries from a public resolver — does not hold the service down, while a blocked `:25` or
broken FCrDNS does. It runs on every start and restart; `TimeoutStartSec=180s` covers it.

## `Type=notify`

`Type=simple` reports the unit started as soon as `ExecStart` forks, which is before the socket is
bound: `systemctl restart verifierd` returns and the very next connection is refused. That is
invisible by hand and fatal to a deploy script, which then cannot distinguish a healthy restart from
a crash loop. `main.go` binds with `net.Listen` first and sends `READY=1` once it is accepting, so
`systemctl restart && curl /readyz` is now a valid deploy check.

## Deploying a new build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /tmp/verifierd ./cmd/verifierd
scp /tmp/verifierd packaging/verifierd.service probe1:/tmp/
ssh probe1 'sudo cp /usr/local/bin/verifierd /usr/local/bin/verifierd.prev
            sudo install -m 755 /tmp/verifierd /usr/local/bin/verifierd
            sudo install -m 644 /tmp/verifierd.service /etc/systemd/system/verifierd.service
            sudo systemctl daemon-reload && sudo systemctl restart verifierd'
```

**Rollback** is the previous binary kept alongside: `sudo cp /usr/local/bin/verifierd.prev
/usr/local/bin/verifierd && sudo systemctl restart verifierd`.

## The boundary

mTLS **and** an API key. The certificates are a private CA issued on the host: `server.pem` carries
`DNS:mail.datascoutmail.com` and `IP:92.222.87.97`, so either form of address works;
`tls.client_ca_file` points at the same CA, so the handshake *requires* a client certificate. Proven
end to end: no certificate → `tlsv13 alert certificate required`; a certificate from another CA →
`tlsv1 alert unknown ca`. Both are TLS alerts, so neither request ever reaches a handler
(invariant 11). With a valid certificate but no API key the answer is `401`.

The CA private key lives at `/etc/verifierd/tls/ca.key`, root-only. It should be moved off the host
to offline storage — a CA whose key sits on the machine it protects buys less than it looks like.

**The API port is deliberately closed at the firewall.** `ufw` allows `22/tcp` and nothing else, so
`0.0.0.0:8443` is reachable only from the host itself. Opening it is plan 008's decision, because
who may reach it is not yet settled: the caller is a Pi on the consumer line whose ISP blocks `:25`,
so its address is probably dynamic, and an allow-rule pinned to it would fail silently the day it
rotates.

## Traps found doing this for real, all silent

- **A proxied `mail.` record breaks FCrDNS.** Cloudflare defaults new A records to proxied, which
  resolves the name to Cloudflare's addresses instead of the host's. `mail.` must be "DNS only".
- **A dual-stack host leaves over IPv6 by default**, bypassing the whole published identity —
  invariant 3, why the prober pins `tcp4`, and why `preflight.sh` now forces IPv4 at every step it
  chooses a path (egress lookup, the `:25` dials, the DNSBL reverse, the live handshake). Before
  that fix it graded the IPv6 identity and called a healthy node NO-GO.
- **A gate that reads the wrong line.** `preflight.sh` matched the RCPT reply with `grep -m1 '^250'`,
  which hits `250-mx.google.com at your service` — the first continuation line of the EHLO greeting.
  It reported a clean `250` whatever the server said, a `5.7.x` block of our IP included.
- **Two `v=spf1` records on one name are a `PermError`, not a merge** (RFC 7208). Cloudflare Email
  Routing wants to add its own; merge by hand into a single record.

## Second node

New IP + rDNS + SPF entry + `probeN` sub-domain; pacing is already node-count-agnostic (ADR-004,
plan 003 put the token bucket in Redis). Note the constraint it brings: greylisting keys on
`(sender, recipient, IP)`, so a retry from a different node is a new tuple and restarts the window.
