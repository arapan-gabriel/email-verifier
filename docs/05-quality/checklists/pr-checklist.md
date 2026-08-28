# PR checklist

Every change confirms all of these before merge. The first five are mandatory and non-negotiable —
they are the invariants that keep this service from becoming a list-destroyer or an open relay.

## Mandatory (invariants)

- [ ] **us ≠ address** — no path turns a rejection of *us* (conn refusal, TLS, timeout, 4xx, 5xx
      before `MAIL FROM`) into `invalid`. When unsure, `unknown`.
- [ ] **SSRF guard** — any new resolution/connection path goes through the guard; no socket to a
      private/loopback/link-local IP.
- [ ] **IPv4 only** — every outbound dial is `tcp4`, never a bare `tcp`. On a dual-stack host a bare
      `tcp` leaves from an address with no FCrDNS and no SPF, and every verdict silently becomes
      `unknown`.
- [ ] **fail closed** — any new pacing/Redis path skips the probe on Redis error; never sends
      unpaced.
- [ ] **central bucket** — no per-process rate state that a second node cannot see; pacing is via
      the shared bucket.

## Correctness

- [ ] Reply→verdict changes are in `internal/prober` only and covered by table tests.
- [ ] `policy` (5.7.x about our IP) and per-recipient deferrals do not drive the pacer.
- [ ] `250` on catch-all/randomiser resolves to `risky`, not `valid`.
- [ ] Suppression checked before any probe/send.

## Gates (match CI exactly)

- [ ] `go test -race -count=1 ./...` green
- [ ] `go vet ./...` clean
- [ ] `gofmt -l .` empty
- [ ] `golangci-lint run` clean

## Docs

- [ ] `06-generated/{api,redis-contract,metrics}.md` updated for any endpoint/key/metric change
- [ ] `changelog.md` entry added
- [ ] Plan Status/DoD updated; moved to `completed/` if done; ROADMAP row updated
