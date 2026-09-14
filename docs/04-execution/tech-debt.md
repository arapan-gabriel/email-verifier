# Tech debt

Open items and their resolution. Add here when a plan defers something; move to Resolved when a
later plan closes it.

## Open

- **`TestConcurrentSessionsAreIsolated` is timing-flaky under load.** Seen once on 2026-09-11 during
  a full `-race` run of all 15 packages — `connections leaked: 1 still active` — and not reproduced
  in five isolated runs or three subsequent full ones. `internal/mxsim/smtp/server_test.go`, present
  since the scaffold and untouched since.

  The assertion reads an active-connection count immediately after closing, so a server goroutine
  that has not yet finished decrementing looks like a leak. It is the test that is racy, not the
  simulator.

  **Worth fixing rather than tolerating:** a gate that fails for no reason is a gate people learn to
  re-run instead of read, and this repository's whole argument for its checklists is that a red
  result means something. The fix is to wait for the count to settle with a deadline rather than
  sampling it once.

- ~~**Invariant 7 has no counterpart in the code.**~~ **Resolved 2026-09-11, by rewording rather
  than by adding a class.** The invariant said a `250` on a catch-all *is* `risky`; the classifier
  has no `ClassRisky` and never had, and four documents named `risky` as a value this service
  produces.

  **Option 2 was taken.** `class` now officially classifies the *reply*, and `risky` is documented
  as the caller's scoring of `class` together with `catch_all`. Adding the class would have been
  truthful to the old wording and would have cost a second cross-repo contract reconciliation — for
  a distinction the caller already reads correctly — on a contract that had just been reconciled,
  exercised over the wire and put into production carrying live verdicts.

  **The invariant keeps its teeth in the place a consumer actually looks.** `06-generated/api.md`
  now states that reading `class` without `catch_all` is a contract violation, with the consequence
  spelled out: a consumer that maps `class` alone marks every address at every catch-all domain
  deliverable. `CLAUDE.md` invariant 7, `smtp-classification.md`, `ARCHITECTURE.md`,
  `ENGINEERING-STANDARDS.md` and `storage-contract.md` all say the same thing now.

- **The prober cannot do STARTTLS, so an MX that requires it is permanently unanswerable.**
  Measured in production on 2026-09-11, warm-up ladder day 3: `gw.art-trier.de` answered
  `530 5.7.0 STARTTLS is mandatory`, which `Classify` correctly calls `policy` — a rejection of
  *us*, invariant 1 — and the caller therefore stores `block: true` with a verdict derived from DNS
  alone. The address is not merely unverified today; it is unverifiable, on this path, for ever.

  `Probe` runs `connect → EHLO → MAIL FROM → RCPT × N → RSET → QUIT` (`prober.go:338`) and has no
  STARTTLS step. The relay does — `sender.go:99-123`, offered-only and deliberately unverified,
  because almost no MX presents a certificate matching the name we dialled (`SECURITY.md`,
  `mail-relay.md`). The same reasoning applies unchanged to the prober; it simply never got the
  code.

  **The fix is two parts, and the first is the one that is easy to miss.** `readReply`
  (`classify.go:299`) walks every continuation line but keeps only the last (`last = line`,
  overwritten each turn), so the EHLO capability list is read off the socket and thrown away — the
  prober cannot see `250-STARTTLS` even when it is announced. That is the exact hazard the relay
  records at `sender.go:202`. So: have the reply reader surface the full text (or the capability
  set), then add the STARTTLS step and the mandatory second EHLO after the upgrade, which the relay
  already does.

  **Why it is worth more than its raw share.** One address in 100 on the day it was found — small.
  But the warm-up ladder's stop rule is *any movement in* `block`, and every such MX contributes a
  permanent, non-reputational `block` to every day's count. It degrades the signal the ladder
  advances on, which is the one number this service exists to keep honest. It is also silent: the
  row looks like every other policy block until someone reads the reply text, which only became
  possible with plan 018.

  **Nothing records a decision to omit it,** so this is read as an omission rather than a trade-off.
  If plain-text probing was in fact deliberate — session fingerprint, cost, anything — that belongs
  in writing here, and the item can be closed as accepted instead of fixed.

- **`ip_health` watches two blocklists, and the one that refused us was not among them.**
  Measured in production 2026-09-12, warm-up ladder day 4: `glowfish.de` answered
  `550 5.7.1 Service unavailable; client [92.222.87.97] blocked using mail.abusix.zone`. At that
  same moment `GET /admin/ip-health` reported `{"burned": false}` and `ip_health_listed` carried
  exactly two series, `zen.spamhaus.org` and `bl.spamcop.net`, both `0`.

  Nothing here malfunctioned. `ip_health.zones` is empty in `verifierd.yaml`, which means the
  built-in default, and the default is those two. But the *meaning* of `burned: false` is
  "the two zones we happen to check are clean", while every caller — and the warm-up ladder's
  stop rule — reads it as "this IP is in good standing". Those differ exactly when it matters:
  the day the IP is listed somewhere else.

  **The first real reputation refusal of the whole ladder arrived through a third party's reply
  text rather than through our own check**, which inverts the design. `SelfTest` exists so the node
  knows its own standing before it spends it; instead the node believed itself healthy while a
  receiving server was reading our IP out of a list by name.

  **Coverage half closed 2026-09-14** (plan 021): the Abusix zone is configured on the node through
  `VERIFIERD_IP_HEALTH_ZONES` and checked alongside the default pair — configured rather than
  defaulted, since it needs a key. **Still open:** a `550` naming our IP feeding back into health,
  and `burned` distinguishing *not checked* from *clean*.

  **Fix, smallest first.** Add `mail.abusix.zone` to the default zone set — it is a major
  operator-grade list and its absence is what this cost. Then the larger point: the ladder's
  stop rule cannot be built on a two-zone sample, so `burned` should distinguish *checked and
  clean* from *not checked*, and a `550` naming our own IP should feed back into health rather
  than dying in a caller's `signals` column. Data Scout already parses that reply text since its
  plan 018 — the code that reads `client [<ip>] blocked using <zone>` is the natural trigger.

  Note Abusix requires a subscription key for DNS queries (`<key>.mail.abusix.zone`), so adding it
  is a credential change as well as a config one, and an unkeyed query answers for everything the
  same way a public resolver does — which the RUNBOOK already warns about for DNSBLs generally.

- **A dropped keyed zone logs its key in the `error` field.** Measured 2026-09-14 during plan 021's
  wrong-key gate: `blocklist zone dropped` printed `"zone":"<key>.combined.mail.abusix.zone"` and,
  in the same line, `"error":"00000000….combined.mail.abusix.zone test point unreachable: lookup
  2.0.0.127.00000000….combined.mail.abusix.zone …"`. `RedactZone` is applied to `d.Zone` in
  `main.go` but `d.Reason` is logged as-is, and `selfTestZone` builds it with the zone (twice: its
  own `%s` and the wrapped `*net.DNSError`'s name). The "no zone passed" error wraps the same text.

  **Why it matters more than a log-hygiene nit:** the case that writes it is exactly a real key
  failing — the free tier's 5,000 queries a *week* running out, or a revoked key — so the key reaches
  the journal on the one restart where someone is reading the journal to find out why.
  **Fix:** build the reasons from `RedactZone(zone)` and redact the wrapped DNS error's `Name`
  (or replace the key label in the final string) before it leaves `iphealth`; test it with a keyed
  zone whose resolver fails.

  Cosmetic, same line: the error says `on 127.0.0.53:53`. The query did go to unbound on
  `127.0.0.1:53` — the resolver's `Dial` in `main.go` ignores the address it is handed — but
  `net.DNSError.Server` reports the `resolv.conf` server, so the log names the resolver this host
  must *not* use for DNSBL. Worth a note in the log text, or wrapping with the configured resolver,
  before it sends someone chasing a misconfiguration that is not there.

- **Bounces to `verify@probe.datascoutmail.com` are discarded** (plan 019). Cloudflare Email
  Routing answers `250` for the sub-domain with no route behind it, which is what makes sender
  callouts pass; anything delivered there is dropped. Harmless while verification is the only
  consumer — the prober never sends `DATA`, so there is nothing to bounce, and the address exists
  to be *looked up*, not to be read. Phase C is where it stops being harmless: plan 015 captures
  bounces and needs a mailbox that exists. Its sender is `noreply@<root>`, not this one, so the fix
  belongs to 015 rather than here — recorded so 015 does not inherit the assumption silently.


Known deferrals baked into the roadmap (not debt, but tracked so they are not forgotten):

- **Shared-queue integration** is deferred behind HTTP (ADR-003); revisit when bulk volume warrants
  it (ROADMAP 007).
- **Multi-node deployment** is deferred to a future ops plan (ADR-004); the pacing model is already
  node-count-agnostic from plan 003, so this is deployment-only.
- **Accept-all resolution** is permanently out of scope (not reproducible) — recorded so it is not
  re-proposed.

## Resolved

- **RESP client has no unix-socket support.** ~~The minimal Redis client ported from the lab in
  plan 003 dials TCP.~~ **Stale — closed 2026-09-04 during plan 013.** It was true of the lab's
  client; plan 003 implemented the branch and never struck the entry. `redis.New` splits a
  `unix:` prefix (`internal/redis/client.go:51`), `config.Redis.Endpoint()` does the same, both
  are tested, and `config/verifierd.yaml` already ships pointing at
  `unix:/run/redis/redis-server.sock`. 013 needs no fallback to `bind 127.0.0.1`.
