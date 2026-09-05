# Tech debt

Open items and their resolution. Add here when a plan defers something; move to Resolved when a
later plan closes it.

## Open

- **Invariant 7 has no counterpart in the code, and four documents describe one that does not
  exist.** Found 2026-09-04 by a live probe of `datascoutmail.com`, which returned
  `{"accepted": true, "catch_all": true, "class": "valid"}`. The invariant says a `250` on a
  catch-all *is* `risky`; `smtp-classification.md:19`, `ARCHITECTURE.md:86`,
  `ENGINEERING-STANDARDS.md:84` and `storage-contract.md:7` all name `risky` as a value this
  service produces. `internal/prober/classify.go` has no `ClassRisky`, and never has.

  Nothing is currently wrong end to end: this service reports `catch_all` as a separate field and
  Data Scout's `scoring.py` scores a catch-all as one (its plan `073`, decision 5). The exposure is
  a consumer that reads `class` and ignores `catch_all` — which is exactly what invariant 7 exists
  to prevent, and it is a *hard* invariant, so the gap is not cosmetic.

  Two ways to close it, and the choice is a product decision, not a cleanup:
  1. **Add `ClassRisky`** and emit it for a `250` from a catch-all or randomising MX. Truthful to
     the invariant; changes the wire contract.
  2. **Reword the invariant and the four docs** to say `class` is a classification of the *reply*
     and that `risky` is the caller's scoring of `class` + `catch_all` together. Truthful to the
     design as built; costs the invariant its teeth unless the mapping is made mandatory.

  **Deliberately not decided during plan 013** — a deployment plan is the wrong place to change a
  published contract, and plan 008 is about to reconcile Data Scout against exactly this shape.
  Settle it before 008 enables the tier, so the reconciliation happens once.


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
