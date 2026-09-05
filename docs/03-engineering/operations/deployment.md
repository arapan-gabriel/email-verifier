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
| `/etc/verifierd/verifierd.yaml` | `root:verifierd 640` | `config/verifierd.yaml` **verbatim** — `diff` against the repo is empty |
| `/etc/verifierd/env` | `root:verifierd 640` | `VERIFIERD_AUTH_API_KEY`, plus the host's own `VERIFIERD_HTTP_ADDR` and `VERIFIERD_TLS_*`. Secrets never in the unit or the repo (invariant 10) |
| `/etc/verifierd/tls/healthcheck/` | `root:root 700` | the node's own client identity, so health checks survive the Data Scout bundle being delivered and removed |
| `/usr/local/sbin/verifierd-deploy` | `root:root 755` | the privileged half of a deploy; the only `sudo` the `deploy` user has |
| `/etc/verifierd/tls/` | `root:verifierd 750` | CA, server pair, `ca.pem` as the client CA |
| `/etc/verifierd/tls/client/` | `root:root 700` | the client bundle Data Scout is issued (plan 008) |
| `/etc/verifierd/dkim/s1.private` | `verifierd:verifierd 600` | DKIM key, phase C |
| `/usr/local/lib/verifierd/preflight.sh` | `root:root 755` | `scripts/preflight.sh` |
| `/usr/local/bin/verifierd-preflight` | `root:root 755` | wrapper supplying the domain, HELO name and DKIM selector |

The installed config is byte-identical to the repository's. It used to be that file with four values
rewritten by `sed` at install time, which is drift waiting to happen — a key added upstream would
never reach the node. All four already had environment overrides (`VERIFIERD_HTTP_ADDR`,
`VERIFIERD_TLS_CERT_FILE`, `VERIFIERD_TLS_KEY_FILE`, `VERIFIERD_TLS_CLIENT_CA_FILE`), so the host's
differences moved into the EnvironmentFile and "what is deployed" became answerable with `diff`.

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

The pipeline (plan 016) is `workflow_dispatch` — a button, not a push trigger. Once plan 008 enables
the tier the node is mid-batch most of the day: a restart cuts in-flight SMTP sessions, and during
the warm-up ladder a deploy on the wrong day smears the measurement the ladder exists to take.

It deploys **the artifact CI tested**, never a rebuild: the workflow resolves the commit's green
`ci` run and downloads its bundle, so a commit whose gate is not green cannot be deployed. The
bundle carries the binary, the unit, `preflight.sh`, the config and `SHA256SUMS`, because a fix that
reaches the binary but not the gate that guards it is worse than no fix.

On the node the privileged half is one root-owned, argument-less script,
`/usr/local/sbin/verifierd-deploy` (source: `scripts/verifierd-deploy`), and it is the *only* thing
the `deploy` user may run through `sudo`. Its exit codes are the interface:

| Code | Meaning | What it did |
|---|---|---|
| 0 | deployed and healthy | restart returned (Type=notify ⇒ accepting) and `/readyz` answered 200 |
| 1 | bad release | restored the previous binary and unit, restarted, confirmed healthy |
| 2 | **bad host** | installed nothing back; left the service down deliberately |
| 3 | refused | checksum mismatch or an incomplete bundle; touched nothing |

**Why 1 and 2 are different is the point of the script.** `ExecStartPre` runs the preflight, so when
the host's own identity breaks — a re-proxied `mail.` record, a fresh blocklist entry, `:25` being
filtered — the *previous* binary fails to start for exactly the same reason. Rolling back would
achieve nothing, cost a second outage to discover, and bury the cause. So the script reads the
journal for the gate's verdict before deciding, and on a `NO-GO` it prints the preflight's own words
and stops.

### By hand (the fallback, and how 013 did it)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /tmp/verifierd ./cmd/verifierd
scp /tmp/verifierd packaging/verifierd.service probe1:/tmp/
ssh probe1 'sudo cp /usr/local/bin/verifierd /usr/local/bin/verifierd.prev
            sudo install -m 755 /tmp/verifierd /usr/local/bin/verifierd
            sudo install -m 644 /tmp/verifierd.service /etc/systemd/system/verifierd.service
            sudo systemctl daemon-reload && sudo systemctl restart verifierd'
```

**Rollback by hand** is the previous binary kept alongside: `sudo cp /usr/local/bin/verifierd.prev
/usr/local/bin/verifierd && sudo systemctl restart verifierd`. The script does this for you on a
code-1 failure.

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
