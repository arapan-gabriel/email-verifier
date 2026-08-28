# Tech debt

Open items and their resolution. Add here when a plan defers something; move to Resolved when a
later plan closes it.

## Open

- **RESP client has no unix-socket support.** The minimal Redis client ported from the lab in
  plan 003 dials TCP. Production (013) wants Redis on `/run/redis/redis-server.sock` — one
  `net.Dial("unix", path)` branch plus a config field. Until then 013 falls back to
  `bind 127.0.0.1`, which works but keeps a loopback TCP hop on the token-bucket hot path.

Known deferrals baked into the roadmap (not debt, but tracked so they are not forgotten):

- **Shared-queue integration** is deferred behind HTTP (ADR-003); revisit when bulk volume warrants
  it (ROADMAP 007).
- **Multi-node deployment** is deferred to a future ops plan (ADR-004); the pacing model is already
  node-count-agnostic from plan 003, so this is deployment-only.
- **Accept-all resolution** is permanently out of scope (not reproducible) — recorded so it is not
  re-proposed.

## Resolved

_None yet._
