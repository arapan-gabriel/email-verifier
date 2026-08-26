# RUNBOOK — from a cold IP to a paced bulk verification

Operational guide for actually running `ratecheck verify` against real MXes.
The tooling ([README.md](README.md)) is the *what*; this is the *in what order*,
and the order matters: every step below is meaningless until the one before it
is done.

The one idea the whole runbook rests on:

> **A real MX judges you on two independent things — your identity and your
> rate — and they fail differently.** Identity (`rDNS`, `SPF`, blocklists) gets
> you rejected *before* the recipient is ever considered, as a `5.7.x` policy
> block that looks like a missing mailbox. Rate (per-MX limits) gets you `421`
> throttled *after* you are accepted. Fix identity first; a rate run against a
> blocked IP measures nothing.

---

## Phase 0 — The sending IP

Everything downstream is a property of the egress IP, so decide it first.

1. **Use a dedicated static IP** you will always send from — not a VPN (shared
   reputation, other tenants, usually against ToS), not a laptop on a home ISP.
2. **Open outbound port 25.** Many hosts block it by default. Open a ticket:
   *"Please enable outbound SMTP (port 25) for email verification / transactional
   mail on IP X; rDNS will be set to mail.yourdomain.com."*

Check:

```bash
timeout 8 bash -c 'exec 3<>/dev/tcp/gmail-smtp-in.l.google.com/25 && echo OPEN' || echo BLOCKED
```

`BLOCKED` here means `verify` cannot run at all — every connection times out.

> Verification is I/O-bound and paced per MX, so **one machine saturates the
> achievable throughput on its own.** Co-locating the verifier with the API / WS
> / database is fine at the start. Do *not* reach for multiple sending nodes for
> speed — see [Probes](#probes-zonds-when-and-how).

---

## Phase 1 — Sender identity in DNS

These are what an MX checks before it will trust the session. `rDNS`/`FCrDNS`
are **per-IP**; `SPF`/`DKIM`/`DMARC` are **per-domain** (one domain covers many
IPs — you do not need a domain per sending node).

3. **rDNS (PTR):** ask the host to set `<your IP>` → `mail.yourdomain.com`.
4. **FCrDNS:** publish an A record `mail.yourdomain.com` → `<your IP>`. Both
   directions must agree:

   ```bash
   dig -x <your IP>            # -> mail.yourdomain.com
   dig mail.yourdomain.com     # -> <your IP>
   ```

   Without this, Yahoo, AOL, Apple, GMX and Microsoft reject every session with
   a `5.7.25`-style policy block before `RCPT`.
5. **SPF** on `yourdomain.com`: `v=spf1 ip4:<your IP> -all` (list every sending
   IP here).
6. **DKIM:** generate a key, publish the public half at
   `<selector>._domainkey.yourdomain.com`.
7. **DMARC** on `_dmarc.yourdomain.com`:
   `v=DMARC1; p=none; rua=mailto:postmaster@yourdomain.com`.
8. **HELO name** = the FCrDNS hostname (`mail.yourdomain.com`), never a lab name
   (`*.test`) — those do not resolve and the `550` back reads like a bad mailbox.

---

## Phase 2 — Get off the blocklists

9. Check the IP: `https://check.spamhaus.org/results?query=<your IP>` and
   `mxtoolbox.com/blacklists`.
10. If listed, **finish Phase 1 first**, then request delisting (state you control
    the IP, it is a mail server, rDNS is set, the issue is resolved).
11. Enrol in postmaster programs (for real sending, but worth having): Google
    Postmaster Tools, Microsoft SNDS/JMRP, Yahoo Sender Hub.

> DNSBL lookups from a public/open resolver (`1.1.1.1`) are refused — they
> answer `127.255.255.254` regardless. Trust the web checker, or the provider's
> own `550` text, over a raw `dig`.

---

## Phase 3 — Preflight until GO

Run the checklist until the verdict is `GO`. Every `[FAIL]` is a hard blocker.

```bash
scripts/preflight.sh yourdomain.com mail.yourdomain.com <dkim-selector>
#   arg1  sending domain (where SPF/DKIM/DMARC live)
#   arg2  HELO name (must FCrDNS to the IP); default mail.<domain>
#   arg3  DKIM selector (optional)
# If egress IP is not auto-detected: IP=x.x.x.x scripts/preflight.sh ...
```

It checks, in order: egress IP, outbound `:25`, rDNS + FCrDNS, HELO resolution,
SPF, DMARC, DKIM, DNSBLs, and a live Gmail handshake — then prints
`PASS/WARN/FAIL` and a `GO` / `GO WITH CAUTION` / `NO-GO` verdict. On `GO` it
prints the exact `-helo` / `-mail-from` flags to use.

**Run it from the same network path `verify` will use** — `:25` reachability and
the egress IP depend on it.

---

## Phase 4 — First real run (Gmail, small, adaptive)

Start narrow. Gmail is the most tolerant of a fresh IP, so it is the honest
first signal.

```bash
./bin/ratecheck verify \
  -file data/probes/gmail.com-probe.txt \
  -limits config/limits-init -group-by mx -domain-parallel 1 \
  -catch-all=false -retries 2 -policy-stop 5 -max-pause 15m \
  -helo mail.yourdomain.com -mail-from verify@yourdomain.com \
  -out data/results-gmail.csv
```

The bands in `config/limits-init/` start the pacer at each provider's ceiling
and AIMD moves inside `[min, max]`: `×0.5` down on a real throttle
(`421`/timeout/reset), `×1.1` up after ten clean answers, never outside the
band. **Per-recipient deferrals — greylisting, `452 4.2.2` over-quota — are
retried but do not drag the shared pacer or arm the pause** (a fix this repo
carries in `ratecheck/internal/verify`).

Read the summary:

| Sign | Meaning |
| --- | --- |
| `BLOCK=0`, low `THROT`, `valid`/`invalid` flowing | identity accepted — good |
| `BLOCK>0`, replies say `blocked` / `Spamhaus` / `5.7.x` / `reverse DNS` | back to Phase 2 — it is your IP, not the rate |
| `THROT` climbing with `421 4.7.x` / `unusual rate` | a genuine Gmail rate limit |

---

## Phase 5 — Find the ceilings, then scale out

### Ladder one MX to its knee

Three terminals. The band file the ladder edits is re-read live by
`-watch-limits`.

```bash
# A — the run
./bin/ratecheck verify -file data/probes/gmail.com-ladder.txt \
  -limits config/limits-probe -watch-limits 15s -group-by mx -domain-parallel 1 \
  -catch-all=false -retries 0 -policy-stop 5 -max-pause 15m \
  -helo mail.yourdomain.com -mail-from verify@yourdomain.com \
  -out data/results-gmail-ladder.csv

# B — climb the rate (conc stays 1 for Gmail)
./scripts/rate-ladder.sh config/limits-probe/gmail.com.json 90 1 1.5 2 3 5

# C — watch only real throttling (quota noise hidden)
tail -n +1 -f data/results-gmail-ladder.csv \
| awk -F, 'NR==1&&$1=="address"{next}
  { c=$4; r=$0
    if(c~/^4/ && r !~ /4\.2\.2|OverQuota|quota/){t++;g="THROT"}
    else if(c~/^5/&&r~/blocked|Spamhaus|5\.7\.|reverse DNS|not attempted/){b++;g="BLOCK"}
    else next
    "date +%H:%M:%S"|getline ts; close("date +%H:%M:%S")
    printf "%s  %-5s THROT=%-4d BLOCK=%-4d  %s\n",ts,g,t+0,b+0,r; fflush() }'
```

Hold each step ≥90s (Microsoft accounts over five minutes; a shorter step
measures nothing). The **first step that produces `THROT` is the limit; the step
before it is the safe ceiling** — record it in `config/limits-init/<mx>.json` as
`max_rate_per_sec`. Concurrency ladders use `scripts/conc-ladder.sh` instead.

Ladder lists hold 50 000 unique addresses so a high step cannot run dry.

### Scale to the rest, one MX at a time

Same shape, swap `-file` / `-out`. Yahoo / Apple / GMX will only answer once
FCrDNS is clean — until then they policy-block on reverse DNS, not on rate.

### Production run over a real list

```bash
./bin/ratecheck verify -file LIST.txt -redis 127.0.0.1:6379 -save-rates \
  -group-by provider -watch-limits 30s -max-pause 15m \
  -helo mail.yourdomain.com -mail-from verify@yourdomain.com -out results.csv
```

- `-group-by provider` — `gmail.com`/`googlemail.com`, `yahoo.com`/`aol.com` etc.
  share one MX, so pace them as one.
- `-save-rates` — resume where the last run settled (a saved rate may only ever
  *lower* the start; the ceiling is earned by clean answers, never by a config).
- `-watch-limits` — `ratecheck apply` retunes the job in flight.

---

## Phase 6 — Store the verdicts

Millions of rows is small for Postgres. **One table, keyed by email, with
`mx_host` as an indexed column — never a table per MX** (the long tail is
thousands of hosts; per-MX tables mean skew, thousands of partitions, and
UNION-across-everything queries).

```sql
CREATE EXTENSION IF NOT EXISTS citext;
CREATE TYPE verdict AS ENUM ('valid','invalid','risky','unknown');

CREATE TABLE email_check (
  email          citext PRIMARY KEY,          -- normalised (lower)
  domain         text        NOT NULL,
  mx_host        text        NOT NULL,        -- the resolved server = pacing key
  provider       text,
  status         verdict     NOT NULL,
  smtp_code      smallint,
  enhanced_code  text,                        -- '5.1.1', '5.7.1' — whose fault
  reply          text,
  attempts       smallint    NOT NULL DEFAULT 1,
  is_catch_all   boolean     NOT NULL DEFAULT false,
  source_ip      inet,                        -- which sending IP produced this
  first_checked  timestamptz NOT NULL DEFAULT now(),
  last_checked   timestamptz NOT NULL DEFAULT now(),
  recheck_after  timestamptz
);
CREATE INDEX ON email_check (mx_host);
CREATE INDEX ON email_check (status);
CREATE INDEX ON email_check (recheck_after) WHERE status <> 'invalid';

CREATE TABLE recipient_domain (
  domain            text PRIMARY KEY,
  mx_host           text,
  provider          text,
  is_catch_all      boolean,      -- true => every 250 here is 'risky'
  catch_all_checked timestamptz,
  mx_refreshed      timestamptz
);
```

Two rules that keep the data honest:

- **A verdict is bound to the sending IP that produced it.** A `5.7.x`/`blocked`
  reply from a listed IP is about *your IP*, not the mailbox — store `source_ip`
  and treat `risky`/`unknown`/`policy` as "re-check from a clean IP", never as a
  fact about the address. Only `valid` / `invalid 5.1.x` are facts about the
  mailbox.
- **Validity decays.** Set `recheck_after`: `valid` → 30–90 days, `invalid`
  5.1.x → 6–12 months, `risky`/`unknown` → `now()` (straight back in the queue).

Load `verify -out` CSVs with `COPY`, upsert `ON CONFLICT (email)` bumping
`attempts` and `last_checked`. The re-check queue is
`SELECT email FROM email_check WHERE recheck_after < now() ORDER BY mx_host` —
already grouped by MX, ready to feed back into `verify`.

Partition only past ~100M rows (`RANGE (last_checked)` for cheap purge, or
`HASH (email)` for even spread) — never `LIST` by MX.

---

## Probes (zonds): when, and how

**Not at the start, and not for speed.** The rate limit belongs to the recipient
MX, and this repo's contract keeps the token bucket **central** in Redis:

> N probes with local buckets means N× the intended rate at Gmail.

So under safe pacing, more sending nodes do **not** raise the allowed rate to a
given MX — one central bucket per MX caps all of them. What probes buy is
**reputation isolation** (one IP blocked, the rest keep going), at the cost of
complexity. Reach for them only when you want that isolation, or you have made a
deliberate choice to send from a larger IP pool.

When you do add them:

- **One domain, one sub-domain per node.** SPF/DKIM/DMARC stay on the single
  domain; each node gets its own `probeN.yourdomain.com` for rDNS + HELO
  (FCrDNS is per-IP). `-mail-from` can stay `verify@yourdomain.com` for all.
- **Share one Redis bucket per MX** (`rt:mx:<host>:bucket`, `token_bucket.lua`),
  take+refill in one round trip, so nodes cannot double-spend a server's budget.
- **Record `source_ip` per verdict** — a policy block on one node does not
  transfer to the others; a `valid`/`invalid` does.

---

## Whose fault is a code?

The enhanced status (`5.X.Y`, RFC 3463) answers "who is this about" before the
prose does:

| Reply | Verdict | About |
| --- | --- | --- |
| `250` | valid (unless catch-all → risky) | the mailbox accepts |
| `550 5.1.1` NoSuchUser | invalid | the recipient |
| `550 5.2.2` mailbox full | valid | mailbox exists |
| `452 4.2.2` over quota | unknown (retry) | the recipient's box, **not your rate** |
| `550 5.7.x` / `554 …blocked` / reverse DNS | policy | **your IP** |
| `421 4.7.x` / unusual rate | throttled | your rate — back off |

`ClassPolicy` (a `5.7.x` about your IP) is never counted as throttling: slowing
down does not grow a PTR record, and if it counted, one blocked IP would
calibrate every provider to zero.

---

## Where things stand / quick triage

| Symptom | Cause | Do |
| --- | --- | --- |
| all connects time out to every `:25` | outbound port 25 blocked | Phase 0 — open `:25` (host ticket) |
| every reply `blocked using Spamhaus` | IP listed | Phase 2 — fix identity, delist |
| Yahoo/Apple/GMX policy-block, Gmail fine | broken FCrDNS | Phase 1 — fix rDNS both ways |
| immediate 10-min pause, CSV empty | conn-level timeouts (bad link/VPN) | raise `-timeout`, drop `-max-pause`; use a stable IP |
| `450 valid` on random name.surname | those are real mailboxes | expected — realistic local-parts hit live inboxes |
| `THROT` from `452 4.2.2` | over-quota counted as rate | already fixed; rebuild `bin/ratecheck` |

**Preflight one-liner before any run:**

```bash
timeout 8 bash -c 'exec 3<>/dev/tcp/gmail-smtp-in.l.google.com/25 && echo OPEN' || echo STILL-BLOCKED
```
