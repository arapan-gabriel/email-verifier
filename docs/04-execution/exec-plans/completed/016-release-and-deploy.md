# Plan 016 — release-and-deploy

**Status:** Complete (2026-09-05)
**Phase:** B
**Depends on:** 013 (the host, the unit, the start gate), 000 (the static-build job)

## Goal

Turn "copy the binary and restart" — the deploy ADR-005 always described and plan 013 performed by
hand — into a repeatable, auditable operation that ships exactly the artifact CI tested, verifies
itself on the node, and rolls back on its own when it does not come up.

## Context

The mechanism was decided long ago and never automated. ADR-005: *"CI publishes the binary as an
artifact. Deploy is copy + `systemctl restart`; rollback is keeping the previous binary alongside."*
Plan 013 carried the task "Preflight as `ExecStartPre` **and CI deploy gate**" and closed with only
the first half built, because a production deploy pipeline is its own decision and needs a
credential nobody had agreed to issue.

What exists now: `ci.yml` gates on push to `main` and on pull requests, then builds the static
binary and uploads it as a workflow artifact. Nothing touches the node. Every byte currently on
`92.222.87.97` was put there over SSH by hand.

What makes this worth doing properly rather than with a three-line `scp` step: the node holds the
isolated sending IP, the CA private key and the API key, and its service refuses to start when the
host's sender identity is broken (`ExecStartPre`, invariant-adjacent — a deploy onto a
reverse-DNS-broken IP produces verdicts that measure nothing). A deploy that cannot tell "the new
binary is bad" from "the host's identity is bad" will roll back the wrong thing.

## Design

### Two decisions, taken 2026-09-05

1. **Trigger: `workflow_dispatch` on a commit whose CI is already green.** Not push-to-main. Once
   plan 008 enables the tier the probe is mid-batch most of the day; a restart cuts in-flight SMTP
   sessions, and during the warm-up ladder (008: 2,000 → 30,000/day, a step a week) a deploy on the
   wrong day smears the measurement the ladder exists to take. The button costs one click and buys
   the choice of moment. *Alternative recorded and not taken:* auto-deploy on merge, which would
   need a deploy window and a drain policy first.

2. **Access: a dedicated `deploy` user with a narrow `sudoers` entry.** The node's only key-holder
   today is `debian`, which has `NOPASSWD:ALL`; putting that key in GitHub secrets would hand any
   workflow — and anyone who can add one — passwordless root on the machine that holds the CA key.
   *Alternative recorded and not taken:* a pull-based deploy (a `systemd` timer on the node checking
   for a new release) needs no inbound credential at all and is the safer shape, but it makes a
   deploy non-immediate and much harder to debug. Revisit if the credential ever looks like a
   liability.

### What actually crosses the wire

The artifact is **the one CI tested**, not a rebuild. The workflow takes a commit SHA (defaulting to
`main`), finds that commit's successful `ci` run, and downloads its artifact. A commit whose CI is
not green cannot be deployed — that is the gate, and it is why the workflow does not build anything
itself. Go builds with `-trimpath` are reproducible enough that a rebuild would almost certainly be
identical, but "almost certainly identical" is not the same fact as "this is the file the tests ran
against".

`ci.yml`'s `binary` job grows to package a small release bundle rather than a bare executable:

| In the bundle | Why it must ship, not be hand-installed |
|---|---|
| `verifierd` | the artifact |
| `packaging/verifierd.service` | 013 installs it verbatim; a change to the unit that never reaches the node is a silent divergence |
| `scripts/preflight.sh` | it is the **start gate**. Plan 013 found three bugs in it; a fix that does not reach the node leaves the gate lying |
| `config/verifierd.yaml` | see *Config drift* below |
| `SHA256SUMS` | a truncated copy is otherwise a broken deploy that looks like a bad build |

### Config drift — removed rather than managed

`/etc/verifierd/verifierd.yaml` on the node is the repo's `config/verifierd.yaml` with four values
rewritten by `sed` at install time (`http.addr` and the three `tls.*` paths). That is drift waiting
to bite: a key added to the repo's config never reaches the node, and the service either refuses to
boot or quietly uses a default.

All four already have environment overrides — `VERIFIERD_HTTP_ADDR`, `VERIFIERD_TLS_CERT_FILE`,
`VERIFIERD_TLS_KEY_FILE`, `VERIFIERD_TLS_CLIENT_CA_FILE` (`internal/config/config.go`,
`applyEnv`). So the fix is to **ship the config verbatim and move the host's four differences into
`/etc/verifierd/env`**, where the API key already lives. The `sed` disappears, the node's config
becomes byte-identical to the repo's, and "what is deployed" becomes answerable with `diff`.

### The privileged half is one root-owned script

`sudoers` entries for primitives (`install`, `systemctl restart`, …) are how a narrow rule quietly
becomes a wide one. Instead the node gets `/usr/local/sbin/verifierd-deploy`, root-owned, `755`,
**taking no arguments**, and `deploy` may run that and nothing else:

```
deploy ALL=(root) NOPASSWD: /usr/local/sbin/verifierd-deploy
```

The workflow's only privileged act is "run the deploy script". What the script does is in the repo
and reviewable; the credential cannot be pointed at anything else.

The script, in order:

1. Verify `SHA256SUMS` against the staged bundle in `/home/deploy/staging/`. Mismatch → refuse,
   touch nothing.
2. Keep `verifierd.prev` and `verifierd.service.prev`.
3. Install the binary, the unit, the preflight script and the config; `systemctl daemon-reload`.
4. `systemctl restart verifierd`. Because 013 made the unit `Type=notify`, this returns **only when
   the socket is accepting** — the restart itself is the readiness check, not a `sleep`.
5. Confirm over the loopback with the client certificate that `/readyz` answers — `is-active` alone
   would not notice a Redis that has gone away (invariant 5: no Redis, no probes).
6. On failure, **distinguish the two causes before acting** (below), then exit non-zero.

### Rollback must not thrash on a host problem

The obvious rollback — restore the previous binary and restart — is wrong for half the failures this
node can have. `ExecStartPre` runs `verifierd-preflight`, so if the host's sender identity breaks
(someone re-proxies the `mail.` record in Cloudflare, the IP gets listed, `:25` starts being
filtered) **the previous binary fails to start for exactly the same reason**. Rolling back then
achieves nothing, takes a second outage to discover, and buries the real cause.

So the script reads the failure before reacting:

- **Preflight returned NO-GO** → the host is the problem, not the release. Leave the previous
  version in place, do **not** restart in a loop, exit non-zero with the preflight's own output. The
  service stays down, which is the correct outcome: probing from a broken identity measures nothing.
- **Preflight passed and the service still did not come up, or `/readyz` failed** → the release is
  the problem. Restore `.prev`, restart, confirm, exit non-zero.

### Workflow shape

- `concurrency: {group: deploy-probe, cancel-in-progress: false}` — two deploys must never interleave.
- `environment: probe1` so the SSH key is scoped to this workflow rather than readable by any job in
  the repository.
- The host key is **pinned** from a repository variable into `known_hosts`. `ssh-keyscan` at deploy
  time is trust-on-first-use on every run, which is no trust at all.
- The runner never talks to `:8443`: the firewall is shut and the endpoint needs a client
  certificate. Every check runs on the node, over SSH.

### What deploy deliberately cannot touch

`/etc/verifierd/env`, `/etc/verifierd/tls/**` and Redis. Secrets and key material are placed once, by
a person, and a deploy has no reason to rewrite them — so it is not given the ability. Note what the
credential *does* imply and accept it consciously: replacing the binary means running code as
`verifierd`, which can read `server.key` and `ca.pem` (group-readable) but **not** `ca.key`, which is
root-only. The CA private key stays outside the deploy blast radius — and per plan 013's note it
should leave the host entirely.

## Tasks

- [x] Node: create the `deploy` user (key-only, no password, own group), `/home/deploy/staging/`
- [x] Node: `/usr/local/sbin/verifierd-deploy` — checksum, keep previous, install, reload, restart,
      `/readyz`, and the two-way failure handling above
- [x] Node: `sudoers.d/deploy` — that one script, `NOPASSWD`, nothing else; `visudo -c` clean
- [x] Node: move `http.addr` and the three `tls.*` paths from the installed YAML into
      `/etc/verifierd/env`; confirm the service comes up identically; drop the `sed` from the docs
- [x] Node: a **separate health-check client identity** so `/readyz` checks survive the Data Scout
      bundle being delivered and deleted from the host
- [x] ~~Repo: have the bundle carry `scripts/verifierd-deploy`~~ — **reversed during implementation,
      it is a privilege-escalation path.** The script stays version-controlled, but shipping it in
      the bundle would let `deploy`, who writes the staging directory, place arbitrary code where
      root runs it. Installing it is a manual, root action; the reason is a comment in `ci.yml`
- [x] **Unit: bound the restart loop** — not in the original task list; see Results
- [x] `ci.yml`: `binary` job packages the release bundle (binary, unit, preflight, config,
      `SHA256SUMS`) instead of a bare executable
- [x] `.github/workflows/deploy.yml`: `workflow_dispatch` with a `sha` input defaulting to `main`;
      resolve that commit's green `ci` run and download its artifact; refuse if it is not green
- [x] Secrets/variables set by the repository owner: `PROBE_SSH_KEY`, `PROBE_HOST`, `PROBE_HOST_KEY`
- [x] Docs: `operations/deployment.md` gains the pipeline and keeps the by-hand procedure as the
      documented fallback; `SECURITY.md` gains what the deploy credential can and cannot reach
- [x] Update `docs/08-decisions/changelog.md`

## Definition of Done

- [x] A commit deploys end to end **from the button** — run against `6a4eb9c`, `EXIT=0`
- [x] The node runs the artifact it was handed, verified by `sha256sum` against `SHA256SUMS`
- [x] **A deliberately broken binary is deployed and rolls itself back**, the service ends up
      healthy on the previous version, and the run is red — done, `EXIT=1`, see Results
- [x] **A simulated host-identity failure does not trigger a rollback** — done, `EXIT=2`, the
      preflight's own `[FAIL]` line printed, the previous binary left alone, see Results
- [ ] Deploying a commit whose CI is not green is refused — **not run.** The predicate it rests on
      was verified against the live API with three inputs (green `main` → a run id; a commit with no
      green `ci` → empty; a nonsense SHA → empty) and the step is `[ -z "$ID" ] && exit 1` around
      it. Left unticked rather than ticked on inference: the workflow path itself has only ever
      taken the success branch
- [x] A tampered bundle (checksum mismatch) is refused before anything is installed — `EXIT=3`,
      the installed binary's hash unchanged
- [x] `deploy` cannot read `/etc/verifierd/tls/ca.key`, cannot read or write `/etc/verifierd/env`,
      cannot `systemctl` anything, cannot run an arbitrary command, and `sudo -l` shows exactly one
      permitted command — all five checked
- [x] The installed `/etc/verifierd/verifierd.yaml` is byte-identical to the repo's
      `config/verifierd.yaml` (`diff` empty), with the host's differences in the EnvironmentFile
- [x] `go test -race -count=1 ./...` green
- [x] `go vet ./...`, `gofmt -l .` clean, `golangci-lint run` clean
- [x] `docs/05-quality/checklists/pr-checklist.md` items confirmed
- [x] Docs updated per `CLAUDE.md` Phase 5; `changelog.md` entry added
- [x] Status set to Complete, plan moved to `completed/`, `ROADMAP.md` row updated

## Results (2026-09-05)

The node half is built and every failure mode is exercised, not reasoned about. Each test ran as the
real `deploy` user through `sudo`, against the live service.

| Test | Result |
|---|---|
| happy path | `EXIT=0`, `sha256` of the installed binary matches the bundle |
| tampered bundle | `EXIT=3`, refused, installed binary's hash **unchanged**, service still up |
| broken binary | `EXIT=1`, rolled back, previous version healthy |
| forced preflight `NO-GO` | `EXIT=2`, **not** rolled back, the `[FAIL]` line printed verbatim |
| `deploy` privileges | `ca.key` unreadable, `env` unreadable and unwritable, no arbitrary `sudo`, `sudo -l` = one command |
| unit change ships | a modified `verifierd.service` reached the node through the script and took effect |
| fresh login | a new SSH session as `deploy` with the new key ran the script to `EXIT=0` |

**Two things the implementation found that the plan had wrong or missing.**

1. **The plan told me to ship the privileged script in the bundle. That is a privilege-escalation
   path and the task was reversed.** The staging directory is writable by `deploy` — that is what
   staging *is* — so a deploy that installed `verifierd-deploy` from it would let `deploy` put
   arbitrary code where root runs it, and the single-line `sudoers` entry would be decoration. The
   script stays in the repository for review; putting it on the node is a manual root action.

2. **`Restart=on-failure` had no limit, and `ExecStartPre` dials Gmail.** The `NO-GO` test left the
   unit in `activating`, retrying every five seconds — meaning that in exactly the situation the
   gate exists to catch, a host whose identity is already broken, the node would open a live SMTP
   session to a real provider every few seconds, indefinitely. That is how a stuck unit turns a DNS
   mistake into a reputation problem, on the one IP this whole project exists to protect. Fixed with
   `StartLimitBurst=3` / `StartLimitIntervalSec=600` and `RestartSec=20s`; re-tested — after 90
   seconds the unit is `failed`, two restarts, `Start request repeated too quickly`. Three
   handshakes total instead of an unbounded stream.

**Config drift is gone.** `diff` between the repo's `config/verifierd.yaml` and the installed one is
empty; `VERIFIERD_HTTP_ADDR` and the three `VERIFIERD_TLS_*` values moved to the EnvironmentFile.

## The pipeline ran (2026-09-05)

Merged to `main` as `6a4eb9c`, `ci` green (run `33970368110`, artifact `verifierd-release`), secrets
and the host-key variable set by the owner, deploy run from the button: **`EXIT=0`**.

The proof it was the pipeline and not another hand-install is the hash. The node had been running
`2591c7c3…`, built on a laptop; after the deploy it runs `83a4435a…`, built by CI, with `2591c7c3…`
kept as `verifierd.prev`. `sha256sum -c SHA256SUMS` in the staging directory returns `OK` for all
four files.

**The deployed binary was then re-verified end to end on the node** — the boundary (TLS alert
without a certificate, `401` without the key, `200` with both), a live `invalid` from Gmail
(`550 5.1.1`) and a live `valid` with `catch_all` from our own domain, both carrying
`source_ip: 92.222.87.97`; metrics populate; `systemctl restart` returns and `/readyz` answers `200`
immediately; `:8443` is unreachable from outside and nothing listens on `:25`.

**Fail-closed was proven against the deployed binary, not just in tests.** With Redis pointed at a
socket that does not exist, `/readyz` returns `503` and a probe of a live address comes back
`class: no_budget` with **`accepted: null`** — not `false`. The one failure this system must never
produce, checked on the real node. The test drop-in was removed and the service restored.

## Residual

One DoD item is unticked: no deploy has ever been *refused*. Everything the refusal depends on is
verified — the API predicate returns empty for a commit without a green `ci` run — but the workflow
has only taken the success branch. A deploy of any pre-merge SHA would close it in one run.

The by-hand procedure in `operations/deployment.md` stays documented as the fallback.

## Notes / decisions / deviations

**This plan is not a prerequisite for plan 008.** The cut-over can proceed on hand deploys; what it
actually needs is the contract fix, the firewall decision and the client bundle. 016 pays off from
the first hotfix that has to land while the tier is live — and the warm-up ladder guarantees there
will be one.

**Why no container, again.** ADR-005 settled it: a `CGO_ENABLED=0` binary already is the immutable,
reproducible artifact an image would wrap. Nothing here reopens that; the bundle is a tarball, not a
registry.

**The pull-based alternative is the better security story and is deliberately deferred.** A node that
fetches its own releases needs no inbound credential, which removes the single worst thing about this
design — an SSH key in GitHub that can restart a production service. It was not chosen because a
timer-driven deploy is slower to land and materially harder to debug at this stage, when deploys are
rare and hand-verified. If the repository ever gains contributors, or the key ever needs to be shared,
that trade-off flips.

**One thing this plan does not solve: nobody scrapes `/metrics`.** A pipeline that deploys safely
still cannot tell you that the release made verdict quality worse. That is a separate gap, recorded
against plan 008's rollout, and the honest limit of what "safe deploy" means here.
