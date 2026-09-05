# Recorded `POST /probe` responses

These are **real responses**, captured from the running service — not examples written from the
prose in `../api.md`.

They exist because Data Scout's side of the integration (its plan `073`) can be written and tested
before the mTLS link is up, and a client written against a document rather than against a live
answer diverges in exactly the details that matter: field names, the semantics of `null`, and the
shape of an error. Parsing these in a unit test pins the contract instead of restating it.

`checked_at` has been normalised to a fixed timestamp so the files are stable; everything else is
verbatim.

| File | Captured against | Shows |
|---|---|---|
| `valid_and_invalid.json` | `mxsim` gmail profile | the ordinary case — a live mailbox and a missing one in one batch, `catch_all:false`, `randomiser:false` |
| `catch_all.json` | `mxsim` catchall profile | `accepted:true` **and** `catch_all:true` — the `250` means nothing |
| `greylisted.json` | `mxsim` yahoo profile | `class:deferred`, `accepted:null`, and `retry_after_seconds` for the caller's scheduler |
| `policy_stop.json` | `mxsim` outlook profile, 8 recipients | 5 attempted, 3 with `err: "not attempted: 5 consecutive policy replies from this server"` |
| `guarded.json` | `mx_host: localhost` | the SSRF refusal — our refusal, never the mailbox's absence |
| `no_budget.json` | Redis unreachable | fail-closed (invariant 5); the MX was never contacted |
| `error_bad_request.json` | a request with no `mx_host` | `400` — a malformed request is never a verification result |
| `error_unauthorized.json` | no bearer token | `401` (invariant 11) |

## `class` is open, and the mapping must treat it that way

New classes appear as the service learns to refuse for new reasons — `guarded` arrived with the SSRF
guard, `no_budget` and `paused` with the pacer, and plan 010 adds one for a burned sending IP. **A
mapping that enumerates the classes it knows will break the first time one is added**, and it will
break by silently mis-scoring an address rather than by raising an error.

The rule that survives every addition:

> Only `valid` and `invalid` are statements about a mailbox. **Anything else is not a verdict** —
> map it to `ProbeResult(connected=…, accepted=None)` and let the existing scoring treat it as
> unconfirmed.

`accepted` already encodes this: it is `null` for every class except those two. A mapping that reads
`accepted` and treats `null` as "unconfirmed" needs no changes when a class is added. One that
switches on `class` needs a default arm, and the default must be "no verdict", never "invalid".

## Reading them

Three fields are **tri-state** and marshal to `null`: `connected`, `accepted`, `catch_all`,
`randomiser`. `null` means the server never gave a usable answer, which is a different fact from
`false`. `policy_stop.json` is the clearest illustration — every recipient has `accepted: null`,
because a server refusing our client said nothing whatsoever about any mailbox.

The mapping onto Data Scout's existing `ProbeResult` is one-to-one for the first three;
`randomiser` is the new signal and is scored like a catch-all.

## Re-recording

Run the service against `internal/mxsim` on a routable address (the SSRF guard refuses loopback, by
design) and capture the bodies. The two error cases and `guarded.json` can be captured locally.
