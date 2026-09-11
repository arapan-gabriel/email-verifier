# Tech debt

Open items and their resolution. Add here when a plan defers something; move to Resolved when a
later plan closes it.

## Open

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
