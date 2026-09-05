# Changelog

One entry per plan (always), newest first: decisions made, deviations, library/provider choices,
trade-offs.

## 2026-09-05 — The firewall this repository documented did not exist

The entry below says the blocker is "one firewall rule". It was not a rule. **There was no packet
filter on the host at all.**

`ufw status` reported `active` with `22/tcp` allowed, and every document here quoted it —
`SECURITY.md`, plan `008` twice, the `013` ROADMAP row, two changelog entries. But `ufw status`
reads `active` from `ENABLED=yes` in `/etc/ufw/ufw.conf`, not from the kernel, and **neither `nft`
nor `iptables` was installed on this Debian 13 host**, so `ufw` could not install a single rule.
`ufw.service` was `inactive (dead)` the whole time.

What actually dropped the traffic was **OVH's edge**, off the box entirely. The measurement that
settles it: from outside, `22` connects, while `80`, `8443` **and `8444` — a port where nothing
listens** — all *time out* rather than being refused. A host receiving those packets would answer
`8444` with an `RST`. It never saw them. A probe to `:8443` from outside also left no line in
`verifierd`'s log, which logs TLS handshake failures.

**So the next step everyone was queued behind was the wrong one.** An `ufw allow from <caller> to
any port 8443` would have printed nothing, added a line to `ufw status`, and changed no packet —
failing exactly the way this plan warns a pinned address would fail: silently, looking configured.
That is the second time in two days that a component was checked against a document instead of
against itself.

**Fixed:** `nftables` installed; `/etc/nftables.conf` written; `nftables.service` enabled so it
survives a reboot; `ufw` disabled outright so the two cannot fight at boot.

```
table inet filter, input policy drop
  ct established,related · lo · icmp/icmpv6 · tcp/22 from anywhere
  tcp/8443 from the Data Scout caller only
  output: accept
```

Three deliberate choices in that ruleset:

* **Output stays `accept`.** This host exists to open outbound SMTP sessions. A stateless ruleset
  that dropped their replies would not protect verification, it would end it — so the
  established/related rule sits first, carrying almost all legitimate traffic.
* **`22/tcp` is not restricted by source.** Pinning SSH to one address, on a box administered from a
  consumer line that changes address, is how a host becomes unreachable with no console. The API
  port is the one that gets a source restriction, because losing it costs a feature rather than the
  machine.
* **ICMP is accepted.** Path-MTU discovery keeps long SMTP sessions alive; dropping it produces
  hangs that read as a slow remote server.

Applied behind a five-minute rollback timer, then verified before the timer was cancelled: outbound
`:25` still reaches Gmail, a *new* SSH connection still lands, `8443` still refuses everyone, and
the policy counter is already collecting scanner traffic.

**What this does not change:** `8443` is still unreachable from outside, because OVH's edge drops it
too. The host filter is now a real second layer, but the edge remains the outer one and it is
configured in a panel, not in this repository. Opening the port end-to-end still needs that side —
and the caller is still down, so the round trip 008 asks for has not happened.

---

## 2026-09-05 — Plan 008's contract blocker is closed; one firewall rule now blocks everything

Cross-repo check, no code here. Data Scout fixed the wire contract on 2026-09-04 (`389f3ae`) and
corrected `073`'s description in the same change, so the prose that caused it cannot re-seed it.
Verified against their current client rather than taking the commit message for it: the payload is
`{mx_host, domain, emails, need_catch_all}`, `results` is read as a map keyed by address,
`valid`/`invalid` drive `accepted`, `catch_all` and `randomiser` are read per result, an unrecognised
class degrades to a non-answer, and `domain` is derived from the batch's first address — sound,
because a batch is one domain by construction and an empty batch returns before indexing.

They also caught something plan 008 had not written down: `POST /probe` is behind our API key as
well as mTLS, so a client certificate alone is a `401`. Their config now carries the token. Our
contract did not move, which was the right call — `api.md` matches the handler field for field.

**The blocker list is now one item: our own `ufw`.** It permits `22/tcp` only, so `:8443` cannot be
reached from the Pi and the single round trip against the live handler — the check that would have
caught the five-way mismatch in the first place — still cannot be attempted. The rule is one line;
the question behind it is not. The caller sits on a consumer line whose address is probably dynamic,
so a rule pinned to it fails silently the day it rotates.

**Updated on their side** (docs and plans only, no code): `073`'s residuals now say mTLS is done and
the firewall is binding; `tech-debt` records that its three remaining items became one-and-a-bit;
the ROADMAP rows reflect a live, pipeline-deployed verifier. Two facts about this service were
written down there because they change how verdicts must be read: **we never emit
`class: "risky"`** — a `250` from a catch-all arrives as `valid` with `catch_all: true`, and their
`scoring.py` already makes the downgrade — and our envelope sender is still the root domain, not the
`probe.` sub-domain, which has SPF but no MX.

**And one document there would have cost an afternoon.** Their cut-over runbook still showed the
`curl` posting `{"mx_host":…,"recipients":[…]}` — the contract that never existed. Run verbatim it
returns `400` and reads as a broken verifier. Now corrected, with the bearer token and the exact
`base64 -w0` commands for the four CD secrets.

## 2026-09-05 — Plan 016: a deploy that can tell a bad release from a bad host

The node half is built and tested; the GitHub half is written and unproven. Trigger is
`workflow_dispatch`, not push-to-main — once 008 enables the tier the probe is mid-batch most of the
day, and during the warm-up ladder a deploy on the wrong day smears the measurement the ladder
exists to take. Access is a dedicated `deploy` user whose `sudo` is one root-owned, argument-less
script; the alternative, handing GitHub the existing `NOPASSWD:ALL` key, would have put passwordless
root on the machine that holds the CA key into repository secrets.

**The design's real content is the difference between exit 1 and exit 2.** `ExecStartPre` runs the
preflight, so when the *host's* identity breaks — a re-proxied `mail.` record, a fresh listing, `:25`
filtered — the previous binary fails to start for exactly the same reason. The obvious rollback
would achieve nothing, cost a second outage to discover, and bury the cause. So the script reads the
gate's verdict out of the journal before it reacts: `NO-GO` means the host is wrong, print the
preflight's own words and leave everything alone; anything else means the release is wrong, restore
and restart. Both paths were exercised against the live service, not reasoned about.

**Implementing it contradicted the plan once and found a defect once.**

- The plan said the bundle should carry `scripts/verifierd-deploy`. **It must not.** The staging
  directory is writable by `deploy` — that is what staging is — so installing the privileged script
  from it would let `deploy` place arbitrary code where root runs it, and the one-line `sudoers`
  entry would be decoration. Task reversed, reason left as a comment in `ci.yml`.
- The forced-`NO-GO` test left the unit in `activating`, retrying every five seconds. `ExecStartPre`
  opens a live SMTP session to Gmail, so **in precisely the case the gate exists to catch the node
  would dial a real provider every few seconds, indefinitely, from an address already failing its
  own identity checks.** That is a DNS mistake becoming a reputation problem on the one IP this
  project exists to protect. `StartLimitBurst=3` / `StartLimitIntervalSec=600`, `RestartSec=20s`;
  re-tested, the unit reaches `failed` after two retries. A gate nobody has watched fail is not
  evidence that failing is safe.

**Config drift removed rather than managed.** The installed YAML was the repo's with four values
`sed`-rewritten at install time, so a key added upstream would silently never reach the node. All
four already had environment overrides, so they moved to the EnvironmentFile and `diff` between
repository and host is now empty.

Also: the node got its own health-check client certificate, separate from Data Scout's bundle, so
`/readyz` checks keep working after that bundle is delivered and deleted from the host.

**Signed off the same day.** Merged to `main` as `6a4eb9c`, `ci` green, secrets set, deploy run from
the button: `EXIT=0`. The proof it was the pipeline is the hash — the node had been running a laptop
build, `2591c7c3…`, and now runs CI's `83a4435a…` with the old one kept as `verifierd.prev`.

The deployed binary was then re-verified on the node, including the thing that matters most: with
Redis pointed at a socket that does not exist, a probe of a live address returns `class: no_budget`
and **`accepted: null`**, not `false`. Fail-closed proven against the artifact a pipeline installed,
not against a test double.

One DoD item stays unticked and is recorded rather than inferred: **no deploy has ever been
refused.** The predicate behind the refusal was checked against the live API — a commit with no
green `ci` run returns nothing, which the step turns into `exit 1` — but the workflow has only taken
the success branch. Ticking it on the strength of two lines of shell would be claiming a test that
was not run.

## 2026-09-05 — Plan 013: deployed, after fixing the gate that guards the deploy

`verifierd` runs on `92.222.87.97` under `systemd`, beside a distro Redis on a unix socket with AOF.
mTLS boundary, preflight as a hard `ExecStartPre`, rollback binary kept alongside. Most of the host
was already right from the sender-identity session; what this plan added is the service, the
boundary and the gate — and most of the work turned out to be in the gate.

**The preflight returned NO-GO on a healthy node**, and the three reasons were all the same shape:
the script reported something other than what it measured.

1. It found the egress with `curl https://ifconfig.me` — over IPv6, on a dual-stack host. So it
   graded an identity with no PTR and no SPF and called FCrDNS broken. **Invariant 3 catching the
   tool written to check invariant 3.** Now IPv4 is forced at every step that picks a path,
   including the `:25` dials, where bash's `/dev/tcp` has no `-4` and must be given the resolved A
   record literally.
2. The DNSBL check reverses the address with `awk -F.`. Handed an IPv6 address it built a name
   nobody publishes, got nothing back, and read nothing as **not listed** — three blocklists
   reported clean for an address never queried. The same false-clean shape as the NXDOMAIN bug in
   plan 010, in a different script.
3. The live handshake read the `RCPT` reply with `grep -m1 '^250'`, which matches
   `250-mx.google.com at your service`, the first continuation line of the EHLO greeting. It would
   have reported a clean `250` whatever the server said — **a `5.7.x` block of our IP included**,
   the one thing the check exists to catch. Read correctly, Gmail says `550 5.1.1 NoSuchUser`.

A gate nobody has ever seen fail is not evidence that it passes.

**`Type=simple` was lying about readiness.** systemd calls the unit started when `ExecStart` forks,
before the socket is bound: a request issued the instant `systemctl restart` returned was refused.
Invisible by hand, fatal to the CD path plan 008 needs — a deploy cannot tell a healthy restart from
a crash loop. `main.go` now binds with `ListenConfig.Listen` before announcing anything and sends
`READY=1`; the unit is `Type=notify`. `sd_notify` is a dozen hand-rolled lines over a unixgram
socket, no dependency, three tests. Binding first also demotes "address already in use" from an
asynchronous surprise to a plain startup error.

**The boundary was proven, not just configured**: `200` with certificate and key, `401` with the
certificate alone, `tlsv13 alert certificate required` with none, `tlsv1 alert unknown ca` with a
foreign one. The last two never reach a handler, which is what invariant 11 asks for.

**A live probe of our own domain exposed a documentation defect.** It returned `class: "valid"` on a
domain with `catch_all: true`. Invariant 7 — a *hard* invariant — says a `250` on a catch-all is
`risky`, and `smtp-classification.md`, `ARCHITECTURE.md`, `ENGINEERING-STANDARDS.md` and
`storage-contract.md` all name `risky` as a value this service emits. `ClassRisky` does not exist in
the classifier and never has. Nothing is wrong end to end — `catch_all` travels alongside and Data
Scout scores it — but the exposure is exactly the naive consumer the invariant was written to stop.
**Recorded in tech-debt with two ways to close it and deliberately not decided here:** a deployment
plan is the wrong place to change a published contract, and plan 008 is about to reconcile Data
Scout against this exact shape. Settle it before 008 enables the tier, so that happens once.

**Two things left undone on purpose.** The API port stays closed at the firewall — the plan wants it
open to "Data Scout's address", and that address is the unsettled question, since the caller is a Pi
on a consumer line and a pinned rule would fail silently when it rotates. And there is no CI deploy
gate: that needs a deploy workflow with SSH secrets, which is its own decision. Both are recorded in
008, which is where the answers live.

Also fixed while deploying: `mail_from` and the `ufw` claim from yesterday's entry are now
reflected on the host, and the CA private key sits on the machine it protects — noted in
`SECURITY.md` as the weak point of this arrangement, to be moved offline before the link carries
production traffic.

## 2026-09-04 — Plan 008 reconciled against the code, and found unrunnable

Checked whether the Data Scout cut-over can start. It cannot, and the reason is the one its own
plan `073` predicted and then did not act on.

**The two sides were written against a document, not against each other.** `073` implemented its
HTTP client from its own prose sketch of `POST /probe` and closed with a warning to reconcile
before enabling the tier. Reconciled: `recipients` vs `emails`, a missing required `domain`, a
`results` list vs a map keyed by address, batch-level vs per-result `catch_all`/`randomiser`, and
`accepted`/`rejected` vs `valid`/`invalid`. Five differences, three of them independently fatal.

The first is not subtle — `handleProbe` sets `DisallowUnknownFields()`, so `recipients` is a 400 on
the first request. What makes this worth writing down is that **every one of the five fails in the
safe direction**: nothing in the mismatch can produce `accepted=False`, so invariant 1 held on both
sides without either side having verified the other. The cost is not a wrong verdict, it is a tier
that looks configured and finds nothing — the failure mode `073` named exactly and shipped anyway.

Our contract does not move: `api.md` matches `internal/api/probe.go` field for field, and it is the
published interface. The fix is one file on the Data Scout side, plus correcting `073` so it stops
re-seeding the mistake. Recorded as a blocker in plan 008 along with a task that no rollout starts
before one real round trip against the live handler.

**008 also gained a dependency it was missing: 013.** The node has no `verifierd` unit, no
`/etc/verifierd/verifierd.yaml`, no binary, and nothing listening but `:22`. There is nothing to
point Data Scout at. The related debt item — "`ufw` leaves the API port open to everyone" — was
simply wrong: `ufw` allows `22/tcp` alone, because the port was never opened.

**Two more measurements changed planned work.**

- `mail_from` still defaulted to `verify@probe.datascoutmail.com`, the address plan 001 proved
  unroutable: SPF TXT but no MX and no A, so a strict receiver answers `554 5.1.8`, which is a
  rejection of *us* and classes `unknown`. Deploying 013 with that default would have produced a
  cut-over that looked broken for a reason no log would name. Now `verify@datascoutmail.com`, in
  config and in the `api.md` example, with the `probe.` MX record written into 013 as the
  precondition for switching back. The identity split is still the design — it is just not live.
- Redis on the node listens on its unix socket only; `127.0.0.1:6379` is refused. **Recorded here
  first as a blocker for 013, on the claim that the RESP client is TCP-only. That claim was wrong**
  — the node was measured, the client was not read. Plan 003 implemented the `unix:` branch
  (`internal/redis/client.go:51`, `config.Redis.Endpoint()`, tested both sides) and left the
  tech-debt entry standing, which is what the claim came from. `config/verifierd.yaml` already
  points at the socket. No fallback to `bind 127.0.0.1` is needed; the debt entry is now Resolved.

**An open decision for 013, not settled here.** Both plans ask that `ufw` admit "the API's address
only". The API is a Pi on the consumer line whose ISP blocks `:25` — the line this whole service
exists to route around — so that address is probably dynamic, and a rule pinned to it would fail
the same silent way as the contract mismatch. mTLS with a stable address in front, or a tunnel:
worth choosing before 013 writes the rule.

## 2026-08-28 — Plan 012: bands widen on evidence, and only by a person

Renamed from `calibration-as-a-service`. Its own note already preferred passive calibration over
provoking `421`s; implementing it turned that preference into the whole design, for three reasons
that were not all true when it was written.

1. **Plan 009 shipped the telemetry.** The knee signal now comes from work we were doing anyway.
2. **The IP is a week old with no sending history.** Ramping until Gmail answers `421` is how a
   fresh address gets listed; RUNBOOK Phase 5 puts laddering after warm-up, deliberately.
3. **The capability is not missing.** `ds-smtp-retry` is a working CLI with the full
   ramp/bisect/soak ladder, runnable from the node. Porting its 615 lines plus the `report` package
   would have added an HTTP trigger — for something the plan says not to run yet — rather than a new
   ability.

**What the loop genuinely cannot do**, and what got built instead: AIMD moves only inside
`[min, max]`, and **all 71 shipped bands say `"confidence": "guess"`**. Gmail's seed is `1.0/s`. If
Gmail tolerates five, this service would sit at one forever and nothing in it would ever notice.

- An MX answering cleanly **at its ceiling** for `pacer.promote_after` probes has demonstrated the
  ceiling is not the limit. The pacer writes a bounded **proposal** to `limits:mx:<host>:proposed`
  with the evidence, and stops.
- **It never applies one.** Lowering is reversible and belongs to the loop; raising a ceiling is
  not, and a band that is too wide fails as a blocklisting rather than as a slow run. One real
  throttle resets the evidence.
- Clean answers *below* the ceiling are not evidence: they say nothing about whether the ceiling is
  the limit.
- Proposals are capped absolutely, so no run of clean answers can propose a rate nobody sanctioned.
  Verified: a band already above the cap produces nothing at all.
- `GET /admin/bands` shows what has been learned; `POST /admin/bands/promote` applies a proposal,
  clears it, and drops the in-memory entry so the next request reads the new band without a restart.
- **Promotion widens the permission, not the rate.** The first test asserted the pacer would resume
  at the new ceiling; it resumes at the rate it had *earned* and climbs from there, because a saved
  rate may only ever lower the start and a ceiling is earned by clean answers rather than granted by
  config. The assertion was corrected, not the behaviour.
- Plan 012 stays **Active** pending manual sign-off.

## 2026-08-28 — Plan 011: suppression, as digests rather than addresses

Reading the Data Scout side changed the plan twice over.

- **It already enforces this, three times.** `privacy_service.is_email_suppressed` is called from
  `verify`, from the prefetch leg, and again in the bulk task — which notes it checks there "rather
  than left to `verify`" precisely so a suppressed address never reaches the engine. What is built
  here is a **second line**, and the fail policy follows from that rather than from taste.
- **The original design would have copied personal data onto this host.** A suppression list is a
  list of email addresses; syncing one here, for a mechanism whose entire purpose is erasure, would
  create the liability the mechanism exists to discharge — on a node contracted through a different
  legal entity, no less. So this service stores **no addresses**: only
  `sha256(salt + "\x00" + value)`. Membership is checkable, the plaintext is not recoverable from
  what sits here, and erasure is deleting one key. An entry that is not a digest is refused rather
  than stored, so an accidental push of plaintext cannot leak. Verified against the node's real
  Redis: two hashes, no `@` anywhere.
- Both keys the source model carries are covered — an address, and a whole domain
  (`suppressions.domain_host` with no `full_name`), which refuses every address on it.
- **Pushed, not pulled.** Data Scout already calls this service; an endpoint here is less machinery
  than an endpoint there plus polling plus credentials pointing the other way. `POST /admin/suppress`
  takes `{version, hashes[], mode}`, where `replace` is what makes a removal at the source
  propagate.
- **Confidence decides consequence**, as in plan 010. On the verify path a missing, stale or
  unreadable copy is loud and non-fatal: the authoritative check already ran, and failing the
  request would trade a real capability for a control that has been applied. Phase C relay will fail
  closed, because sending is irreversible and has no upstream check between the queue and the socket.
- **Invariant 9 is not weakened by that.** It says a suppressed address is never probed or mailed,
  and it holds. What fail-open acknowledges is that this copy is a redundancy.
- Enabling suppression without a salt **refuses to boot**: an empty salt would not fail, it would
  silently make every lookup miss — the worst possible way for this particular check to be broken.
- Refusal happens before the SSRF guard, before the budget and before any socket, and costs the rest
  of the batch nothing: a suppressed address and a live probe went out in the same request, one
  refused and the other answered.
- Plan 011 stays **Active** pending manual sign-off.

## 2026-08-28 — Plan 010: IP health, designed around the false positive

Standing the node down automatically is the point of this plan and also its danger: **a false
positive is a self-inflicted outage.** Three ways to get one, all measured on our own address rather
than imagined, and each shaped a decision.

- **A DNSBL query through a stub answers "listed" for every zone.** The deployed node's resolver is
  `systemd-resolved` on `127.0.0.53` — exactly that case. So checking is **off unless
  `ip_health.resolvers` names one explicitly**, with no fallback, and whatever is named must pass a
  **self-test** against each zone's documented test points (`2.0.0.127` listed, `1.0.0.127` not)
  before a single real answer is acted on. Verified against the real stub: it is refused, the
  service still starts and serves, and a probe afterwards returns `no_budget` — not `ip_burned`.
- **UCEPROTECT L3 lists a whole ASN.** Our address is on it because AS16276 is, while Spamhaus,
  SpamCop and UCEPROTECT L1/L2 were clean and both Gmail and Microsoft accepted the session. No
  delisting clears it. It is not in the default zones, and the reason is recorded where someone
  would otherwise add it.
- **One server refusing us is not the IP being burned.** Policy replies are counted across
  *distinct* MX hosts and raise a signal an operator reads — they never pause. Pausing on them would
  hand any misconfigured or hostile MX a way to stand the node down. Plan 007 already stops probing
  a single server that refuses us.
- **Confidence decides consequence**: a confirmed listing from a self-tested resolver pauses the
  node; an inference only alerts; a failed query is ignored, because a failure is not a listing.
- A burned node refuses probes with `class:ip_burned`, `connected:false`, `accepted:null` — our
  refusal, never a verdict. `GET /admin/ip-health` shows the standing and
  `POST /admin/ip-health/resume` clears a pause **without a redeploy**, because the check can be
  wrong and the cost of being wrong is answering nothing.
- `ip_health_listed{ip,list}` joins the registry; it is absent until a check has run, and absent
  means checking is off.
- Documented alongside: `class` is an open set, and a caller's mapping must treat anything that is
  not `valid` or `invalid` as no verdict. `ip_burned` is the fourth such class and will not be the
  last; a mapping that enumerates them breaks by silently mis-scoring rather than by raising.
- **A bug the unit tests could not have found.** The first live run failed the self-test against a
  resolver that demonstrably works: NXDOMAIN is how a DNSBL says "not listed", Go reports it as an
  error, and the query treated any error as an unreachable zone. Every *clean* zone therefore read
  as broken, the self-test failed permanently, and blocklist checking would never have run in
  production — silently, with the only symptom a log line blaming the resolver. The fakes missed it
  because they returned `(nil, nil)` for "not listed", which no real resolver does. Fixed by reading
  `net.DNSError.IsNotFound`; regression tests now use the error a real resolver returns, and assert
  that a genuine `SERVFAIL` is still a failure rather than quietly clean.
- Verified end to end afterwards against a DNSBL-capable resolver, using Spamhaus's own documented
  test address as the burned case: our address comes back clean and probes normally; `127.0.0.2`
  burns the node, refuses probes with `ip_burned`, and raises `ip_health_listed` on both zones;
  `resume` clears it with no redeploy.
- Plan 010 stays **Active** pending manual sign-off.

## 2026-08-28 — Plan 009: observability, and the memory leak it uncovered

- **No Prometheus client library.** It pulls protobuf, procfs and expfmt into a repository with one
  dependency, to render a text format for a fixed set of metrics. `internal/metrics` renders it
  directly, as this repository already does for RESP and for SMTP. The one thing worth care by hand
  is the histogram — cumulative `le` buckets, `+Inf`, `_sum`, `_count` — and it is tested against a
  known distribution.
- **The unbounded pacer map.** Working out what to label the gauges with surfaced a real leak:
  `Pacer.mx` is keyed by `mx_host`, which arrives in the request, and nothing evicted it. A bulk run
  over ten thousand domains held ten thousand entries for the life of the process — and every
  per-MX metric labelled from it would have been a time series that never went away. One fix serves
  both: idle eviction plus a cap, lossless because the working point is in Redis, so an evicted
  entry costs one re-read rather than a reset to the ceiling. `verify_tracked_mx` reports the
  ceiling being approached, which is the metric to alert on.
- Two entries in the contract described things that no longer exist: `verify_requests_total{status}`
  counted verdicts this service stopped producing under ADR-006. It is now
  `verify_results_total{class}`, over the classes that are actually returned. `ip_health_listed`
  moves to plan 010, arriving with the state it reports.
- Gauges are **pulled from the pacer at scrape time** rather than mirrored: it owns that state, and
  a copy would give two answers that can disagree.
- `GET /metrics` goes through the same guard as any non-health route (invariant 11) — an operator
  surface on a public IP is still a surface.
- **Request id at the edge**, echoed in `X-Request-Id` and present in every log line. A
  caller-supplied id is honoured, so a trace spans Data Scout and this service.
- **No address at info level**, with a test. The domain and a count are enough to find a problem;
  the local part is the customer's data.
- A visible side effect worth recording: after three guarded requests to `localhost`,
  `verify_tracked_mx` reads `1`, not `4`. The earlier fix that moved the SSRF guard ahead of the
  budget means a refused target creates no pacer entry and therefore no time series.
- Plan 009 stays **Active** pending manual sign-off.

## 2026-08-28 — The SSRF guard now runs before the budget is taken

Found by walking the manual verification rather than by a test. With Redis unreachable, probing
`mx_host: 127.0.0.1` returned `no_budget` instead of `guarded`: the pacer ran first, so the guard was
never reached.

The ordering was wrong for two reasons beyond the confusing output:

- **A guarded target spent a token.** Budget is for questions actually put to a server, and that one
  was never going to be contacted.
- **It created an attacker-influenced Redis key.** `mx_host` comes from the request, so the bucket
  `rt:mx:127.0.0.1:bucket` was written on the caller's say-so. Bounded by the script's one-hour
  expiry, but still unbounded key cardinality driven by request input.

Resolve and vet now run first; the budget is taken immediately before the socket. A refusal we make
ourselves is free and touches nothing shared, and a Redis outage can no longer mask an SSRF refusal
behind a fail-closed one. Two regression tests: a guarded target takes zero tokens, and the guard
still fires when `Acquire` would have failed.

## 2026-08-28 — Plan 007: policy-stop, all that was left of the bulk plan

Renamed from `bulk-verify-and-queue`. ADR-006 left exactly one behaviour: the bulk endpoint, the job,
the group-by-MX runner and the results retrieval are Data Scout's Celery task, and the
transport-agnostic engine entrypoint the plan asked for is how `internal/prober` was built anyway.

- **After `probe.policy_stop` consecutive `ClassPolicy` replies the session ends**, and the rest of
  the batch comes back `connected:false`, `class:policy`, `not attempted: …`. A server that decides
  against our client decides it for the whole session, so continuing spends a token per recipient on
  an answer already known — up to forty-nine of them in a fifty-recipient batch — while hammering a
  server that has just said no, which is how a soft block hardens.
- **Consecutive, not cumulative**, and `1` is refused at startup. A single `5.7.x` can be a
  per-recipient policy — a distribution list rejecting external senders — and stopping a batch on
  one reply would throw away the rest of the answers. The counter resets on any non-policy reply.
- Catch-all probing is skipped once the stop trips: a server refusing us cannot tell us which local
  parts exist.
- **The pacer sees none of it** (invariant 6), asserted. Slowing down does not grow a PTR record, and
  if policy counted as throttling one blocked IP would calibrate every provider to zero.
- **Remembering the refusal across requests is deliberately left to plan 010.** That is the same
  signal as "our IP is burned", it belongs with the IP-health state and the alert, and the right
  response there may be to pause the node rather than one server. A second overlapping mechanism
  here would only let the two disagree.
- **Signed off 2026-08-28.** Complete and moved to `completed/`. **Phase A is closed** apart from
  plan 008, which is cross-repo and waits on mTLS material and coordination with Data Scout.

## 2026-08-28 — Cross-repo: Data Scout's plans brought in line (its ADR-009)

Reviewed Data Scout's active plans against everything decided here. One of the seven was
**superseded outright**, and it was the one that mattered.

- **Its plan `073` was going to route `aiosmtplib` through a SOCKS5 proxy** on a small VPS, keeping
  the probe inside the API process. Its own decision 2 named moving the probing subsystem onto the
  VPS and declined it as "not warranted to prove one IP works" — which is exactly what this
  repository then did. The plan has been rewritten around the service that now exists, and
  **ADR-009** was added on that side to record why, paired with ADR-006 here.
- Dropped from it: the SOCKS5 daemon, `python-socks`, `verify_smtp_proxy_url`, and the
  proxy-versus-policy-routing decision. Kept: the `ENGINE_VERSION` bump, fail-closed, the staged
  warm-up ladder, and the CD path for the flags.
- Carried across from decisions made here: the seam is `smtp_probe.probe_many` (ADR-006), a
  transport failure maps to `connected=False` and never to a verdict, the greylist retry stays on
  their side scheduled by `retry_after_seconds` with the `(sender, recipient, IP)` tuple constraint
  written down (plan 006), `randomiser` is scored like a catch-all and is a property of the server
  (plan 005), and their four fixed per-MX ceilings retire in favour of the AIMD bucket while the
  platform-wide daily cap stays as a quota (plan 003).
- Also updated there: `tech-debt.md`'s port-25 fix, the `073` ROADMAP row, `071`'s reference to a
  "warmed relay pool" — which is now a second probe node and deliberately out of scope until one is
  proven — and a changelog entry.
- Plan 008 here now points at `073` as its other half rather than restating it, and lists what this
  repository actually owes the cut-over: the CA and certificates, `tls.client_ca_file` set so the
  handshake requires one, and `ufw` narrowed to the caller.
- **Nothing was committed in the Data Scout repository** — those changes are staged in the working
  tree for review, and that work runs in its own session.

## 2026-08-28 — Plan 006: the greylist queue belongs to the caller

The open question this plan had carried since ADR-006 — "a deferred retry has no synchronous caller
to return to" — is resolved, and not by preference. **A retry this service performed by itself would
produce a verdict with nowhere to go.** It is stateless about business data (ADR-003) and owns no
jobs (ADR-006), so the row, the job and the quota are all Data Scout's, and Data Scout is the only
place an answer can land. It already has Celery with exponential backoff. Pacing survives the move
for free: a retry is just another `POST /probe` through the same bucket.

Renamed from `greylist-retry-queue`; no queue, no callback, no result store — any of them would make
this service stateful about who asked.

- **`retry_after_seconds`** on every class that means "come back later" (`deferred`, `throttled`,
  `no_budget`, `paused`), so the caller schedules instead of backing off blindly into a window that
  has not opened. Blind exponential backoff retries seconds later and burns a token to be told the
  same thing.
- For `paused` the hint is **exact** — `pacer.PausedError` now carries the remaining cooldown.
  Otherwise it is parsed from the reply when the server offers a number, and `probe.deferral_retry`
  (15 minutes) when it does not.
- **Clamped to [60s, 6h].** A server claiming "retry in 3 seconds" would have the caller burn a
  token before the window opens; one claiming "in 30 days" would have it abandon a live address.
  The parse itself is deliberately narrow — reading intent out of SMTP prose is guesswork, and the
  configured default is a perfectly good answer.
- An answered address carries no hint. Attaching a retry to a `valid` or an `invalid` would invite
  the caller to re-ask a question that has been answered.
- **The tuple constraint is now written down**, in `patterns/retry-greylist.md` and plan 008:
  greylisting keys on `(sender, recipient, IP)`, so a retry from a different node or with a
  different `MAIL FROM` is a new tuple and restarts the window — forever, if the caller keeps
  rotating. Automatic with one node; a routing constraint the moment there are two, and one nothing
  in this service can enforce.
- Verified against `mxsim`'s greylisting profile with its clock advanced: first sighting defers with
  a hint, the same tuple after the window is accepted and carries no hint.
- `appendonly yes` stays required in `redis-contract.md` even though the queue it was justified by
  is gone — the calibrated bands and the settled rate are worth keeping across a crash, and enabling
  it cost nothing.
- `452 4.2.2` is left classified as a deferral although, like `550 5.2.2`, it implies the mailbox
  exists. The enhanced code travels with the result so Data Scout can score that inference where the
  rest of the scoring lives; asserting it in the classifier would change the lab's measured
  behaviour on the strength of an argument rather than a measurement.
- **Signed off 2026-08-28.** Complete and moved to `completed/`.

## 2026-08-28 — Plan 005: telling a catch-all from a coin flip

Renamed from `catch-all-and-classification`; two of its four design points were already gone. The
**per-domain catch-all cache** is Data Scout's — `email_domain_profile_service.knows_catch_all`
exists and decides `need_catch_all` before it calls here — and the **status reconciliation table**
is moot under ADR-006, where this service returns facts and Data Scout scores them.

- **N bogus probes, not one** (`probe.catch_all_probes`, default 3, refuses to boot below 2). All
  accepted → catch-all. All rejected → the real replies stand. **Anything in between → randomiser.**
  One probe cannot tell the third case from the first two: it lands on accept or reject by coin
  flip, so the same domain reports catch-all on one run and clean on the next, and a real mailbox
  behind it is reported valid on a `250` that meant nothing.
- **`internal/mxprofile`** remembers a randomiser verdict per **MX host** (`mx:<host>:randomiser`,
  TTL 24h). That scope is the point: a catch-all is one domain's business, a randomiser is the
  server's, so it condemns every domain behind it — including ones nobody has asked about yet. A
  later request for a different domain on that host carries the verdict and sends no probes at all.
- The response gains `randomiser`. A randomiser also sets `catch_all: true` — the conservative
  reading, and the field existing callers already handle correctly, so a consumer that does not yet
  know the new field still refuses to trust the `250`.
- **The bogus probes cost budget** like any other recipient: asking three questions spends three
  questions' worth.
- A profile-store failure degrades to "probe again", not to a failed request. Unlike the rate budget
  this costs accuracy rather than safety, and failing would trade a real answer for no answer.
- Documented where it will be looked for: `smtp-classification.md` gains the scope table,
  `features/002` is written up properly, `redis-contract.md` gains the key, and the reconciliation
  section is marked superseded rather than left to rot.
- `mxsim` has no randomising profile — its chaos knobs produce random 4xx deferrals, not random
  accept/reject — so the coin-flip host is covered by a scripted dialer. Noted as a possible
  follow-up rather than a reason to change ported code.
- **Signed off 2026-08-28.** Complete and moved to `completed/`.

## 2026-08-28 — Plan 004: resolver control and an in-process cache

Rewritten before it was implemented. As planned it was MX discovery, priority sorting, implicit-MX
fallback and a "no mail server" verdict — all of which ADR-006 had already moved to Data Scout, and
plan 002 had already built the resolution and the guard. Implementing the original tasks would have
meant building against a boundary that no longer existed.

- **Configurable resolvers** (`dns.servers`) and a resolution timeout of its own. Previously the
  service used whatever `/etc/resolv.conf` said and inherited only the HTTP deadline, so a slow
  resolver could spend the probe's whole budget before a socket opened. On the node the host's
  resolver is `systemd-resolved` on `127.0.0.53` — fine for A records; the RUNBOOK's warning about
  stubs bites on DNSBL lookups, which is why the knob exists and why plan 010 will want it.
- **An in-process TTL cache**, positive and negative, size-capped. Deliberately not Redis: the node
  already runs a caching resolver, so a Redis round trip to avoid a lookup the OS has cached would
  make the hot path slower, not faster. What it buys is that the service does not fall over when
  deployed somewhere `resolv.conf` points straight at a public resolver and a 500-domain bulk job at
  one provider means 500 identical queries.
- **Only vetted results are cached, and refusals are cached too.** Caching the raw answer would be a
  way to smuggle a refused address back past the SSRF guard; caching the refusal means a domain
  pointing its MX inward costs one lookup rather than one per request. Refusals expire faster than
  answers, so a misconfiguration that gets fixed is not remembered as long as a good result.
- **`dns:mx:<domain>` dropped from the Redis contract** with the reasoning recorded there: a DNS
  answer is not state that has to be shared between nodes. Redis keeps what genuinely must be — the
  rate budget, the bands, IP health.
- **No `miekg/dns`.** The one thing the standard library cannot give is the record TTL, and a fixed
  conservative TTL is adequate for the A records of MX hosts. The sanctioned dependency stays
  unused until something needs it.
- Every cache assertion counts lookups performed rather than elapsed time; expiry is driven by
  `synctest`. Smoke-tested on the node afterwards: a real Gmail MX still resolves and answers
  correctly, and `localhost` is still refused by the guard.
- **Signed off 2026-08-28.** Complete and moved to `completed/`.

## 2026-08-28 — Plan 003: the central token bucket becomes the limiter

- **`internal/redis`** — the wire codec is ported from the lab; the connection handling is not. That
  client dials a fresh TCP connection per command, which is fine for a calibration CLI and wrong
  where take+refill runs once per probe. New here: pooling, unix sockets, `context`, and
  `EVALSHA` with an `EVAL` fallback so a kilobyte of Lua is not shipped on every probe.
- **`internal/limiter`** — take and refill in one Lua call against `rt:mx:<host>:bucket`. With two
  round trips concurrent workers read the same token count and both spend it, which is how "one
  request per three seconds" quietly becomes N per three seconds (invariant 4).
- **`internal/pacer`** — AIMD over that bucket: start at the band ceiling, halve on a real throttle,
  climb 10% after ten consecutive clean answers, never leave `[min,max]`, and stand the MX down for
  its cooldown when the floor still is not enough.
- **Invariant 6 is now a signature, not a comment.** `Observe(ctx, mxHost, throttled bool)` — the
  pacer never sees a `Class`, and the prober passes `Class.IsThrottle()`. A deferral (greylisting,
  `4.2.2` over-quota) or a `5.7.x` policy block cannot reach it even by mistake. That mattered
  enough to encode: three full mailboxes or one blocked IP would otherwise drag a provider to zero.
- **One token per recipient**, not per session — batching under ADR-006 must not spend less budget
  than asking one at a time.
- Bands resolve most-specific-first: `limits:mx:<host>` in Redis, then the shipped seed for the
  recipient domain, then a conservative default. A saved runtime rate may only ever *lower* the
  start: backing off is a measurement, a quiet hour below the ceiling is not evidence the ceiling
  moved.
- **The Lua script and all 71 seed bands are embedded** (`go:embed`). The artifact is one file
  (ADR-005), and a limiter that fails because a file is missing would fail open in the worst place.
  `internal/limiter/token_bucket.lua` and `internal/pacer/bands/` replace `config/limiter/` and
  `config/limits/`; the doc references were updated with them.
- `/readyz` now issues a real `PING` instead of dialing — with no Redis there is no budget and every
  probe fails closed, so such a node should stop receiving work.
- **Verified on the node against real Redis**: a 12-recipient batch against a `[0.5..2]/s` band ran
  at **1.99/s**, and AIMD settled at the floor in `BACKOFF` after `mxsim` throttled. With Redis
  stopped: `/readyz` 503, every address `no_budget` / `connected:false` / `accepted:null`, and
  **zero connections opened to the MX**. Redis back → recovered with no restart.
- An accidental run as the wrong user hit the Redis socket's `660 redis:redis` permissions and
  produced exactly the same clean refusal — a misconfiguration that could have meant unpaced sending
  instead failed closed.
- `appendonly yes` + `appendfsync everysec` enabled on the node, as `redis-contract.md` requires
  before plan 006.
- Known bound for the multi-node plan: the bucket is shared so nodes cannot double-spend, but the
  rate each node passes is its own in-memory value persisted to `rt:mx:<host>:rate`, so nodes
  converge rather than agree instantly.
- **Signed off 2026-08-28.** Complete and moved to `completed/`.

## 2026-08-28 — Plan 002: SSRF guard between the lookup and the socket

- **The hole was concrete, not theoretical.** Plan 001 built the target as
  `net.JoinHostPort(req.MXHost, port)` and handed the *name* to the dialer, which resolved it
  itself — leaving no place for a guard to sit. `internal/prober` now takes `netip.Addr` values and
  dials an IP literal, so no second, unguarded resolution can happen underneath it.
- **New `internal/resolver`**: A-record resolution of the supplied `mx_host` plus a deny-by-default
  guard. Anything that is not routable global unicast is refused; the named ranges then cover what
  the standard-library predicates miss — carrier-grade NAT, benchmarking, the documentation blocks,
  IETF assignments, and the IPv6 tunnelling prefixes (6to4, Teredo, NAT64, v4-mapped) that can embed
  an arbitrary IPv4 address and carry it inward. Over-blocking costs a rare "not attempted";
  under-blocking is a hole.
- **The guard is the prober's default, not an option.** Forgetting to wire one cannot produce an
  unguarded prober — a test asserts the dialer is never called for an internal target.
- **IP literals are vetted without a lookup**, so `mx_host: "127.0.0.1"` does not depend on what a
  resolver chooses to do with a literal.
- **New class `ClassGuarded`** — added, not ported, since the lab connects to loopback on purpose.
  Neither `IsTemp` nor `IsThrottle`: retrying changes nothing and slowing down does not change
  someone's DNS record. Plan 009's `verify_probe_blocked_total{reason="ssrf"}` reads off it.
- A guard refusal (`*BlockedError`) is a different error from a DNS failure, so the caller can tell
  "we would not go there" from "DNS did not answer".
- Resolution is `ip4` only (invariant 3): asking for AAAA would only produce addresses we must
  refuse to leave from.
- Verified over HTTP with no resolver configured: `127.0.0.1`, `169.254.169.254`, `10.0.0.5`,
  `::ffff:127.0.0.1` and the *name* `localhost` all came back `class:guarded`, `connected:false`,
  `accepted:null`, each with its reason. From the deployed node, `gmail-smtp-in.l.google.com` still
  resolved and answered correctly — the guard is not a denial of service against ourselves.
- Integration tests bypass the guard through one explicit `loopbackResolver`, because the fake MX
  runs on loopback. Bypassing it visibly in a named type beats weakening the guard to make tests
  pass.
- **Signed off 2026-08-28.** Complete and moved to `completed/`.

## 2026-08-28 — Plan 001: the prober, and `POST /probe`

- **Classifier ported verbatim** from `../ds-smtp-retry/ratecheck/internal/prober` into
  `internal/prober/classify.go` — the reply→class map, `IsTemp`/`IsThrottle`, the RFC 3463 reader
  and the hint lists, unchanged apart from doc comments the linter required. It is deliberately kept
  close to upstream so measurements made in the lab still describe this code, and it is the single
  place in the service where a code becomes a meaning.
- **New batched session** (`internal/prober/prober.go`): connect → EHLO → MAIL FROM → RCPT × N →
  one bogus RCPT for catch-all → RSET → QUIT, per ADR-006. `DATA` is never sent (invariant 8).
  Batches split at `max_rcpt_per_session`; catch-all is probed once per request because it is a
  property of the domain, not of a chunk.
- `Connected`/`Accepted`/`CatchAll` are tri-state pointers marshalling to `null`, mapping one-to-one
  onto Data Scout's existing `ProbeResult`. **Only `ClassValid` and `ClassInvalid` set `Accepted`;**
  every other class leaves it nil, which is how invariant 1 is carried in the type rather than in a
  convention someone has to remember.
- **Invariant 3 is now enforced, not documented.** `probe.dial_network` must be exactly `tcp4` or
  the service refuses to boot; `tcp`, `tcp6`, `udp` and empty are all rejected, with a test.
- `POST /probe` behind `Options.Authenticated`, so the route cannot be registered without the guard.
  The handler declares its own one-method `Prober` interface (ENGINEERING-STANDARDS §2), so the API
  tests use a three-line fake and open no sockets.
- **mTLS implemented but not enabled**: `tls.{cert_file,key_file,client_ca_file}` →
  `RequireAndVerifyClientCert` with TLS 1.3 minimum. The CA and certificates come with plan 013;
  until then the API key guards the route and startup warns about both plain HTTP and
  TLS-without-client-certs. A half-configured listener (cert without key, CA without cert) is a
  boot refusal, because it would otherwise serve plain HTTP on a port the caller believes is
  protected.
- **`auth.enabled` now defaults to true.** An authenticated route exists from this plan on, so the
  edge is guarded by default and the service will not boot without a key (invariant 11).
- Integration tests drive `internal/mxsim` in-process on an ephemeral port: gmail profile gives
  `valid`/`invalid` correctly and `catch_all:false`; the catch-all profile gives `catch_all:true`;
  the 11th connection in the rate window returns `class:throttled` with `connected:false` and
  `accepted:null`. That last one is the whole point — the same `421`, arriving in the banner before
  `MAIL FROM`, must be simultaneously "not a verdict about this mailbox" and "the one signal that
  moves the pacer".
- Found while testing: the teardown `RSET`/`QUIT` could block on a server that stops reading. The
  answers are already collected by then, so both writes now get a short write deadline — a tarpit
  must not be able to hold a session, and its slot in the rate budget, open for the full timeout.
- `probe.port` added (25 in production) so a staging instance can be aimed at the lab MX.
- **First run against real MXes found an invariant-1 violation in the ported classifier.** A
  `554 5.1.8` about our envelope sender marked two live mailboxes `invalid`: RFC 3463 puts `X.1.7`
  and `X.1.8` in the "addressing" subject next to the recipient codes, but they are about the
  *sender*, and `classifyPermanent` read all of subject 1 as a statement about the recipient. Both
  added to `senderCodes` (checked first), sender wording added to the hints, regression test added.
  **`../ds-smtp-retry` still carries this bug** — port the fix back.
- **`probe.datascoutmail.com` is not a routable sender domain.** It carries only the SPF TXT record;
  a server doing sender-domain verification finds no MX and no A and rejects every probe with
  `5.1.8`. Confirmed by switching to `verify@datascoutmail.com`, which the same server accepts. The
  fix is an MX on the sub-domain pointing at the same Cloudflare routers as the root — verified that
  Cloudflare answers `250` to a `RCPT` for it, so sender callouts pass and not just DNS lookups.
  Until then the sub-domain isolation from ARCHITECTURE §"Sender identity" is not in effect.
- **Consumer `gmail.com` does distinguish a real mailbox from a missing one.** Measured: our own
  address `250 2.1.5`, a random local part `550 5.1.1 NoSuchUser`. `build-vs-buy` §4 assumes Gmail
  "answers identically to an existing and a non-existing mailbox" and prices the project on that
  assumption. That is not what consumer Gmail did here. Google Workspace-hosted domains are a
  separate case and were not tested — but §4's estimate of what a self-hosted probe is worth looks
  too pessimistic and should be re-checked before it is used for planning again.
- **Signed off 2026-08-28.** Plan 001 is Complete and moved to `completed/`. Phase A continues at
  plan 002 (ssrf-guard-and-safety), which is what makes the endpoint safe to expose at all.

## 2026-08-28 — ADR-006: the seam is `probe_many`; no jobs here; Redis persistence required

Three corrections found by reading the Data Scout side properly before starting plan 001, rather
than after plan 008 would have forced them.

- **ADR-006 accepted**, superseding ADR-003's transport shape and bulk handling. Its
  "stateless about business data" decision is unchanged and carried further.
- **The seam is `smtp_probe.probe_many`, not the provider.** Data Scout already resolves MX and
  groups addresses by domain (its plan 067, done deliberately so "500 addresses no longer mean 500
  connections"). Cutting at `verify(email)` would have issued one HTTP request per address and
  destroyed that grouping. The contract is now `POST /probe {mx_host, domain, emails[],
  need_catch_all}` → per-address results mapping one-to-one onto the existing `ProbeResult`.
- **No jobs and no bulk endpoint on this service.** Data Scout's Celery already owns chunking,
  progress, per-row quota metering, the result artifact and a reaper for stalled jobs. Duplicating
  it here meant two definitions of "done" — and ROADMAP 007 never said what resumed an in-process
  bulk worker after a restart. With no jobs here a redeploy costs at most one in-flight chunk, which
  the caller retries. Plan 007 shrinks to policy-stop alone; plan 004 narrows to A/AAAA + the SSRF
  guard (MX discovery stays in Data Scout); plan 008 becomes a change to one function plus mTLS.
- **A transport failure between the two services is `unknown`, never `invalid`.** Invariant 1
  governs the HTTP hop exactly as it governs SMTP — a probe redeploy mid-request must not be able to
  condemn a mailbox. Recorded in ADR-006, `api.md`, `ARCHITECTURE.md` and plan 008.
- **Authentication is mTLS**, with an API key as a second factor inside the tunnel for operator
  routes. The host is on a public IP with `:25` open and is scanned continuously; mTLS ends the
  handshake before a request reaches the application.
- **Redis persistence is now a requirement, not a default.** Measured on the deployed node:
  `appendonly no`, RDB only (`save 3600 1 300 100 60 10000`), so a crash or power loss drops up to
  an hour of writes. Tolerable for calibrated bands, which are re-learned by design — **not**
  tolerable for plan 006's greylist retry queue, whose whole promise is surviving a restart. A
  losing retry means an address is never re-asked and the caller waits forever. `appendonly yes` +
  `appendfsync everysec` added to plan 006, plan 013 and `redis-contract.md`.
- Open question flagged in plan 006: with no job here to attach it to, a deferred retry has no
  synchronous caller to return to. The option most consistent with the architecture is that the
  retry queue is not this service's concern at all — return `class:deferred` and let Data Scout
  re-queue. To be resolved before that plan is implemented.

## 2026-08-28 — Coding patterns fixed in ENGINEERING-STANDARDS, and retrofitted

Chosen for what this service actually has to get right, not for novelty. Each rule is tied to an
invariant it protects, and plan 000's own code was rewritten to follow it rather than only
documenting it.

- **Errors are classified by type, never by message text** (§4). `errors.Is`/`errors.As`, sentinels,
  `%w` wrapping; `strings.Contains(err.Error(), …)` is forbidden. Invariant 1 — "a rejection of us is
  never a verdict about the address" — is carried entirely by how failures are typed, and a string
  match rots the moment a provider rewords a reply. The failure mode is deleting a live mailbox.
  Applied now: `config.ErrInvalid` plus `errors.Join` so validation reports every problem at once.
- **Interfaces are declared by the consumer, one or two methods** (§2). This is what makes the test
  seams the docs already demanded actually cheap. `api.ReadinessFunc` is the pattern at its
  smallest; plan 001's handler will own a one-method `Verifier`.
- **Dependencies are passed in; no package-level mutable state, no `init()`** (§2). ADR-004 forbids
  pacing state a second node cannot see, and a package-level variable is exactly that.
- **`main` is a wrapper around `run(ctx, args, getenv, stderr) error`** (§2). Startup, graceful
  drain and bad-config paths are now covered by tests that spawn no process.
- **`context.Context` first on anything doing IO**, never stored in a struct (§5).
- **`testing/synctest` for all time-dependent tests** (§7), GA in Go 1.25 and verified working in
  this module: six simulated minutes elapse in 0.00 s. Cooldowns, greylist backoff and bucket refill
  are asserted deterministically instead of slept through. **The lab's hand-rolled `Clock` is not
  ported into the service** — `synctest` fakes `time` itself, so production signatures stay clean.
  `internal/mxsim` keeps its own clock only because it is ported code kept close to upstream.
- Also codified: constant-time credential comparison, `Options` structs over functional options,
  `t.Context()` over `context.Background()` in tests, `getenv` injection over `t.Setenv`.
- Propagated into `testing/{strategy,unit}.md`, `pr-checklist.md` (six new review items) and the
  plans where the rules bite: 001 (typed reply classes, consumer-side `Verifier`), 003, 006 and 012
  (`synctest` for pacing, retry backoff and calibration timing).

## 2026-08-28 — Plan 000: scaffold, gates, systemd unit, mxsim ported

- **First code.** Module `github.com/arapan-gabriel/email-verifier`, Go 1.25, one dependency
  (`gopkg.in/yaml.v3`, pulled in by config parsing and the ported mxsim profiles).
- `internal/config` — the single config source: defaults → optional YAML → `VERIFIERD_*` env, then
  validation. The service refuses to boot on a bad or missing required value instead of starting
  half-configured; `auth.enabled` without `auth.api_key` is one of the refusals.
- `internal/api` — router, canonical `{"error":{"code","message"}}` shape, `GET /healthz`,
  `GET /readyz`, JSON 404 for anything else. `Options.Authenticated` is the seam plan 001 hangs
  `POST /verify` on, so no route can quietly skip the guard (invariant 11).
- `cmd/verifierd` — config load, `log/slog` JSON logging, graceful drain on SIGTERM/SIGINT.
- **`mxsim` ported** from `../ds-smtp-retry/mxsim` into `internal/mxsim/{smtp,policy,clock,metrics,
  admin}` plus `cmd/mxsim`, profiles in `config/mxsim/`. It was pulled into this plan because plans
  001, 003, 006 and 012 all name it in their Definition of Done and none of them tasked porting it —
  001 would have been blocked on unplanned work. Its tests pass unchanged in this module; it is
  linted loosely on purpose (`.golangci.yml`) so upstream fixes stay easy to apply.
- Also ported: `scripts/preflight.sh`, `config/limiter/token_bucket.lua` into the location the docs
  already cite, and the seed rate bands as `config/limits/` (71 provider files).
- `Makefile` carries the static build flags (ADR-005: the binary is the artifact); `make gate` is
  the Phase-4 gate verbatim. CI runs the same four steps and then asserts the built binary is
  statically linked.
- `packaging/verifierd.service` — hardened unit with `LimitNOFILE=65535`, SIGTERM drain inside
  `TimeoutStopSec=30s`, `RestrictAddressFamilies=AF_INET AF_UNIX` (which also happens to enforce
  invariant 3 at the kernel level), empty capability set. Verified with `systemd-analyze` on the
  real host.
- `readyz` dials the configured Redis endpoint rather than issuing a real `PING` — the RESP client
  arrives in plan 003. Recorded in `api.md` so the contract does not overstate what is checked.
- Gate green: `go test -race`, `go vet`, `gofmt -l`, `golangci-lint run`. Full results in the plan.
- **Signed off 2026-08-28.** Plan 000 is Complete and moved to `completed/`; the ROADMAP row is
  closed. Phase A execution continues at plan 001 (http-verify-service).

## 2026-08-28 — Invariants renumbered 1–11; IPv4-only promoted to a hard invariant

- **`CLAUDE.md`'s hard-invariant list is now 1–11 with no gaps or letters.** The old list ran
  `1, 2, 2b, 3 … 9`, and `AGENTS.md` mirrored it with the `2b` flattened away — so the two files
  disagreed on every number from 3 onward, and two docs had already drifted onto the wrong one:
  `patterns/smtp-classification.md` and plan 010 both cited "invariant 5" for policy-≠-throttle,
  which was 4 in `CLAUDE.md` and 5 only in `AGENTS.md`. Both corrected.
- **IPv4-only is now invariant 3**, promoted from a plan-level note. It sits next to the SSRF guard
  because the two are the same question from opposite ends: guard 2 governs which address we connect
  *to*, invariant 3 governs which address we connect *from*.
- Old → new: `2b`→4, 3→5, 4→6, 5→7, 6→8, 7→9, 8→10, 9→11. Invariants 1 and 2 unchanged. Every
  citation across `docs/` was remapped in the same change; `Data Scout invariant 10` references
  belong to the other repo's numbering and were deliberately left alone.
- `AGENTS.md` now states that it only mirrors `CLAUDE.md` and the two renumber together — the
  ambiguity that caused the drift.
- `pr-checklist.md` gains IPv4-only to its mandatory block (now five items, not four);
  `SECURITY.md` gains the matching line.

## 2026-08-28 — Sending node provisioned; sender identity live; IPv4-only pinned

- **Node deployed and verified.** OVH VPS-1 (2 vCore / 4 GB, France), Debian 13, `92.222.87.97`,
  domain `datascoutmail.com`. Outbound `:25` open, no inbound MTA, SSH key-only, ufw (inbound 22,
  **outbound allowed** — a `deny outgoing` default would silently kill port 25), chrony synced,
  unattended-upgrades on, Redis on a unix socket with TCP disabled (`port 0`), `verifierd` user and
  directories created. Full state and results recorded in plan 013's Definition of Done.
- **Sender identity published and confirmed against live MXes.** FCrDNS agrees both ways; SPF on the
  root and on `probe.`; DMARC with `sp=none`; DKIM selector `s1` (RSA-2048). Gmail and Microsoft
  both answered `250 2.1.5` on `RCPT` with no policy block. No `DATA` was ever sent
  (`swaks -4 --quit-after RCPT`).
- **New architectural invariant: IPv4 only.** Measured, not theoretical — on this dual-stack host a
  plain `net.Dial("tcp", ...)` chose IPv6 and reached Gmail from `2001:41d0:404:200::169b`, an
  address with the provider-default PTR and no SPF coverage. Providers accept the connection and
  reject at `RCPT` with `5.7.x`, which this service classifies as `ClassPolicy` — so every probe
  would return `unknown` while looking like a classifier defect. Plan 001 now pins `dial_network:
  tcp4` as an explicit config value with a test; recorded in `ARCHITECTURE.md` §Invariants.
- **Sender-identity split recorded** (`ARCHITECTURE.md` §"Sender identity", plan 013,
  `operations/dns.md`): HELO is always `mail.<domain>` (it must FCrDNS), while `MAIL FROM` differs —
  `verify@probe.<domain>` for verification, `noreply@<domain>` for Phase C relay. `probe.` carries
  its own SPF because a receiving MX checks the `MAIL FROM` domain's SPF against the connecting IP
  during a `RCPT` probe. This keeps probing reputation off the domain that will carry customer mail.
  The RUNBOOK, ported from the lab, still assumes a single `verify@yourdomain.com` for everything.
- Two silent traps documented in plan 013 Notes and `operations/dns.md`: a CDN-proxied `mail.` A
  record breaks FCrDNS (Cloudflare proxies new A records by default), and a dual-stack host leaves
  over IPv6 unless the address family is pinned.
- UCEPROTECT L3 lists the IP, but ASN-wide (AS16276 / OVH), not the address; Spamhaus ZEN, SpamCop
  and UCEPROTECT L1/L2 are clean, the `/24` sampled clean, and both providers tested accept the
  session. Re-provisioning would not clear an ASN-level listing — recorded so it is not re-raised.
- Still open: Email Routing for the DMARC `rua=` mailbox (MX currently empty, so reports are lost),
  and `scripts/preflight.sh` as the formal gate — the script is ported in plan 000, so the checks
  above were run by hand.

## 2026-08-26 — Deployment form: systemd, no container runtime

- **ADR-005 accepted** — deploy as a systemd service, not a container. Plans **000** and **013**
  rewritten to match. The release artifact is a single `CGO_ENABLED=0` static
  binary installed under `systemd` (`packaging/verifierd.service`), with Redis from the distro
  package on a unix socket or loopback. The original `Dockerfile` + `docker-compose.yml` tasks are
  dropped, along with the `docker compose up` item in 000's Definition of Done.
- Reasons are project-specific, not stylistic: Docker's bridge SNAT would mask the egress address
  that `source_ip` must report; `172.16/12` is exactly what the SSRF guard rejects (invariant 2);
  and a bridge hop on the token-bucket path adds a failure mode to a path that is fail-closed by
  invariant 5. A `CGO_ENABLED=0` binary has no dependencies to isolate.
- `systemd` sandboxing (`ProtectSystem=strict`, `NoNewPrivileges`, empty `CapabilityBoundingSet`, …)
  replaces the hardening a container would have provided; `LimitNOFILE=65535` covers the concurrent
  SMTP session count from ADR-001.
- Plan 013 gained concrete host guidance from the provider survey: full region only (an OVH **Local
  Zone** can never reach any SMTP port, with no unblock path), IP and `/24` reputation verified
  before the host is built on, plain Debian 13 with no control panel, and no inbound MTA.
- Plan 013 now also depends on 000 (it installs 000's `packaging/verifierd.service`).
- Deferred to tech-debt: unix-socket support in the ported RESP client.
- ADR-005 explicitly leaves ADR-004 intact: a static binary is identical across nodes by
  construction, so "add node #2" stays a deployment step.

## 2026-08-26 — Repo bootstrap & architecture

- Created `email-verifier` as a standalone Go service repo with the Data Scout docs layout.
- **Architecture locked** via ADRs 001–004:
  - ADR-001 — build on the `ds-smtp-retry` Go engine (mature AIMD/bands/classifier/central bucket).
  - ADR-002 — scope = verification probe + outbound relay, phased (verify first, relay in Phase C);
    layers 0–5 stay in Data Scout.
  - ADR-003 — HTTP integration now (queue later); stateless about business data (Data Scout stores
    verdicts).
  - ADR-004 — one probe node at the start; the token bucket is central from plan 003 so scaling is
    deployment-only.
- Seeded docs: `CLAUDE.md`, `README.md`, `ARCHITECTURE.md`, `ENGINEERING-STANDARDS.md`, `AGENTS.md`,
  `DOCS_GUIDE.md`, product index, generated contracts (`api`/`redis-contract`/`metrics`), patterns
  (`smtp-classification`/`aimd-pacing`/`ssrf-guard`/`retry-greylist`), pr-checklist, ROADMAP, plan
  template, and plan 000.
- Ported references from `ds-smtp-retry`: `RUNBOOK.md`, `token_bucket.lua`; and the Data Scout
  build-vs-buy analysis (the decision record mandating a separate isolated-IP probe).
- No code yet — execution starts at plan 000 (scaffold-and-standards).
