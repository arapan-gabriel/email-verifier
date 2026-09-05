# Operations — IP reputation & health

**Live** (plan 010). The IP is the asset; protecting it is continuous ops.

## What runs

`internal/iphealth` queries the configured blocklist zones for the sending address on a ticker and
keeps the verdict in `ip:health:<ip>`. A confirmed listing stands the node down: probes come back
`class:ip_burned`, `connected:false`, `accepted:null` — our refusal, never a verdict.

`GET /admin/ip-health` shows the standing; `POST /admin/ip-health/resume` clears a pause without a
redeploy. The next scheduled check re-evaluates, so resuming overrides a wrong verdict rather than
turning the checking off.

## Why it is off by default

Standing the node down automatically is the point, and it is also the danger: **a false positive
here is a self-inflicted outage.** Three ways to get one, all measured rather than imagined:

- **A DNSBL query through a stub answers "listed" for every zone.** The deployed node's resolver is
  `systemd-resolved` on `127.0.0.53`, exactly that case. So checking requires `ip_health.resolvers`
  to be set explicitly — there is no fallback — and the resolver must pass a **self-test** against
  each zone's documented test points (`2.0.0.127` must come back listed, `1.0.0.127` must not)
  before a single real answer is acted on. A resolver that fails disables checking and logs an
  error; it never pauses anything.
- **UCEPROTECT L3 lists a whole ASN.** Measured 2026-08-28: our address is on it because AS16276 is,
  while Spamhaus ZEN, SpamCop and UCEPROTECT L1/L2 were clean and both Gmail and Microsoft accepted
  the session. No delisting clears it. It is **not** in the default zones and should not be added.
- **One server refusing us is not the IP being burned.** A `5.7.x` from one MX is that host's
  opinion — plan 007 already stops probing it. Policy replies are counted across *distinct* hosts
  and raise a signal an operator reads; they never pause the node. Pausing on them would hand any
  misconfigured or hostile MX a way to stand the node down.

## Confidence decides consequence

| Signal | Confidence | Action |
|---|---|---|
| Confirmed listing, self-tested resolver | high — someone published a fact about our address | **pause the node**, alert |
| Policy replies across several distinct MXes | an inference | **alert only** |
| A failed DNSBL query | none | ignored — a failure is not a listing |

## Standing obligations

- rDNS/FCrDNS must stay valid; a PTR change is an incident.
- Verification traffic looks like harvesting to providers — pacing (plan 003) is what keeps the
  address clean. Delisting and identity setup: `docs/07-references/RUNBOOK.md`.
- Enrol in the postmaster programmes when real mail starts flowing: Google Postmaster Tools,
  Microsoft SNDS/JMRP, Yahoo Sender Hub.
