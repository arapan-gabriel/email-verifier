# Plan 011 — suppression-enforcement

**Status:** Complete (2026-08-28)
**Phase:** B
**Depends on:** 001

## Goal

Never probe or mail an address Data Scout has suppressed (GDPR erasure / opt-out). The suppression
list is honoured before any socket opens.

## Context

Data Scout owns suppression (its `suppressions`, feature 005 / plan 040). Invariant 7: an address
Data Scout has suppressed is never probed or mailed. This service reads a synced copy so it can
enforce locally without a round trip per address.

## What changed since this was written

**Data Scout already enforces it, three times.** `privacy_service.is_email_suppressed` is called
from `email_verification_service.verify`, from the prefetch leg, and again in the bulk task — which
notes it checks there "rather than left to `verify`" precisely so a suppressed address never reaches
the engine. This service is therefore a **second line**, not the control.

That reframes both the design and the risk.

## The thing the original design got wrong

A suppression list is **a list of email addresses**. Syncing it here means holding personal data at
rest on a second host — for a mechanism whose entire purpose is erasure, and on a node contracted
through a different legal entity. Copying it would create, in the name of GDPR compliance, exactly
the liability GDPR is about.

So: **this service never stores an address.** It stores salted **hashes**. Membership is checkable,
the plaintext is not recoverable from what is stored here, and erasure is deleting a hash. The salt
is configured and shared, so both sides compute the same digest.

## Design

- `internal/suppress` — a hashed set in Redis, checked before resolution and before any probe.
  A suppressed address is `class:suppressed`, `connected:false`, `accepted:null`, with an auditable
  reason — never probed, and in Phase C never mailed.
- Both keys the source model carries are covered: an **address** and a whole **domain**
  (`suppressions.domain_host` with no `full_name`). Each is hashed with the same salt.
- **Pushed, not pulled.** Data Scout already calls this service; adding an endpoint here is less
  machinery than an endpoint there plus polling plus a second set of credentials.
  `POST /admin/suppress` takes `{version, hashes[], mode}` — `replace` for a full export, `add` for
  an increment.
- **Confidence decides consequence**, as in plan 010:
  - Verification: the local set is a second line and the first has already run. If it is missing,
    stale beyond a threshold, or unreadable, **log loudly and continue**. Failing the request would
    take the service down for a secondary control.
  - Relay (Phase C): **fail closed.** Sending is irreversible, and there is no upstream check
    between the queue and the socket.

## Tasks

- [x] `internal/suppress` — salted-hash membership over Redis, address and domain
- [x] `POST /admin/suppress` — versioned push, `replace` and `add`
- [x] Check before resolution; `class:suppressed` with a reason
- [x] Staleness: a threshold, a loud log, and **no** request failure on the verify path
- [x] Config: salt (required when suppression is enabled), staleness threshold
- [x] Tests: address hit, domain hit, miss, stale set does not block, replace vs add, no address is
      ever written to Redis
- [x] Update `redis-contract.md`, `api.md`, `SECURITY.md`, `service/storage-contract.md`, changelog

## Definition of Done

- [x] A suppressed address is refused before any socket, with a reason
- [x] A suppressed **domain** refuses every address on it
- [x] **No email address is ever written to Redis** — asserted by inspecting what the store received
- [x] A missing or stale set logs loudly and does **not** fail the request
- [x] `replace` clears what a previous export left; `add` does not
- [x] Enabling suppression without a salt refuses to boot
- [x] `go test -race`, `vet`, `gofmt`, `golangci-lint` clean
- [x] Status → Complete, moved to `completed/`, ROADMAP row updated

## Results (2026-08-28)

Gate clean. End to end on the node, against real Redis:

```
import  {"version":"export-2026-08-28","size":2,"stale":false}

arapan.gabriel.v2@gmail.com          class=suppressed  accepted=None  suppressed by address
zz9q7x-no-such-box-5501@gmail.com    class=invalid     accepted=False
anyone@gone.test                     class=suppressed                 suppressed by domain

SMEMBERS suppress:hashes
  9c36a1fa7d54a5cc3ebb94c859aa320836e0f3c6f5ee86b73eb6996bc2c1e472
  ef74fa1b5dd20abe1c0a440f9a16a8ee03be19ae0e3af94efd7d16e873871634
addresses among them: 0
```

The second row is the one worth reading twice: a suppressed address and a live probe went out in the
**same batch**. One was refused before any socket; the other got its real answer. Suppression costs
its neighbours nothing.

| Check | Result |
|---|---|
| Suppressed address | refused before the guard, the budget and any socket; zero tokens spent |
| Suppressed domain | every address on it refused |
| What Redis holds | two digests, no address, no `@` anywhere |
| An address pushed instead of a digest | refused, not stored |
| `replace` | clears the previous export; `add` does not |
| Unreadable list | request proceeds, failure reported to the caller-supplied hook |
| `enabled` without a salt | refuses to boot |
| `/admin/suppress` without credentials | `401` |

## Notes / decisions / deviations

Source of truth is Data Scout; this is enforcement, not ownership, and the sync is one-directional.

**Invariant 9 is not weakened by the fail-open choice on the verify path.** The invariant says a
suppressed address is never probed or mailed, and it holds — enforced upstream, where the list
lives, and again here when the local copy is available. What fail-open acknowledges is that this
copy is a redundancy: making the whole service refuse to answer because a redundancy is stale would
trade a real capability for a control that has already been applied. The relay path in Phase C has
no upstream check between the queue and the socket, which is why it fails closed instead.

**Hashes are still personal data** under GDPR — pseudonymised, not anonymised. The point is not that
the obligation disappears but that the blast radius shrinks: what sits on the probe node cannot be
read back into a mailing list, and erasure is a delete of one key rather than a hunt through a
second datastore.
