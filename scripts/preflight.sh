#!/usr/bin/env bash
# preflight.sh -- "before the first run" checklist for a sending IP.
#
# Verifies the sender identity a real MX judges you on BEFORE you point
# ratecheck at anything: outbound :25, rDNS/FCrDNS, SPF/DKIM/DMARC, blocklists,
# and a live SMTP handshake. Prints GO / NO-GO and what to fix.
#
#   scripts/preflight.sh <sending-domain> [helo-name] [dkim-selector]
#   IP=203.0.113.7 scripts/preflight.sh yourdomain.com mail.yourdomain.com s1
#
# <sending-domain>  the domain in -mail-from (SPF/DKIM/DMARC live here)
# [helo-name]       EHLO name; must FCrDNS to the IP. default: mail.<domain>
# [dkim-selector]   optional; checks <selector>._domainkey.<domain>
#
# Run it from the SAME network path ratecheck will use (same host/VPN state),
# because :25 reachability and the egress IP depend on it.
set -uo pipefail

DOMAIN=${1:-}
[ -z "$DOMAIN" ] && { echo "usage: $0 <sending-domain> [helo-name] [dkim-selector]"; exit 2; }
HELO=${2:-mail.$DOMAIN}
SELECTOR=${3:-}

pass=0; warn=0; fail=0
ok(){   printf '  [ OK ] %s\n' "$*"; pass=$((pass+1)); }
wn(){   printf '  [WARN] %s\n' "$*"; warn=$((warn+1)); }
er(){   printf '  [FAIL] %s\n' "$*"; fail=$((fail+1)); }
hd(){   printf '\n== %s ==\n' "$*"; }

# Resolver that is not the local stub (127.0.0.53 refused DNSBL for us).
DNS=1.1.1.1
digq(){ dig +short "@$DNS" "$@" 2>/dev/null; }

# ---------------------------------------------------------------- egress IP
hd "Egress IP"
IP=${IP:-}
if [ -z "$IP" ]; then
  # -4 is not a preference, it is the whole measurement. The sending identity
  # (PTR, FCrDNS, SPF) is published for the IPv4 address alone; a dual-stack
  # host answers over IPv6 by default, so without this the script reports on an
  # identity the prober never uses and calls a healthy node NO-GO. Same reason
  # the prober pins tcp4 -- invariant 3.
  IP=$(curl -4 -s --max-time 8 https://ifconfig.me 2>/dev/null)
  [ -z "$IP" ] && IP=$(curl -4 -s --max-time 8 https://api.ipify.org 2>/dev/null)
fi
if [ -z "$IP" ]; then
  er "could not determine egress IP (pass it: IP=x.x.x.x $0 ...)"
  IP="0.0.0.0"
elif ! echo "$IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$'; then
  # Refuse to grade a non-IPv4 egress rather than measure the wrong identity.
  er "egress '$IP' is not IPv4 -- every check below would grade an identity the prober never uses"
  IP=0.0.0.0
else
  ok "egress IP: $IP  (this is who the MX sees; must match your rDNS/SPF)"
fi

# ------------------------------------------------------------- outbound :25
hd "Outbound port 25"
open25=0
for h in gmail-smtp-in.l.google.com mx.yandex.ru smtpin.zoho.com; do
  # Dial the A record literally: bash's /dev/tcp has no -4, and on a dual-stack
  # host it would otherwise prove that *IPv6* egress works.
  h4=$(getent ahostsv4 "$h" 2>/dev/null | awk 'NR==1{print $1}')
  h4=${h4:-$h}
  if timeout 8 bash -c "exec 3<>/dev/tcp/$h4/25" 2>/dev/null; then
    ok ":25 -> $h reachable"; open25=$((open25+1))
  else
    er ":25 -> $h BLOCKED/timeout"
  fi
done
[ "$open25" -eq 0 ] && er "port 25 is blocked outbound -- verify cannot run at all"

# --------------------------------------------------------------- rDNS/FCrDNS
hd "Reverse DNS (rDNS) and FCrDNS"
PTR=$(digq -x "$IP" | head -1 | sed 's/\.$//')
if [ -z "$PTR" ]; then
  er "no PTR for $IP -- Yahoo/Apple/GMX/Microsoft reject before RCPT. Ask host to set rDNS."
else
  ok "PTR: $IP -> $PTR"
  FWD=$(digq "$PTR" A | head -1)
  if [ "$FWD" = "$IP" ]; then
    ok "FCrDNS confirmed: $PTR -> $IP (matches)"
  else
    er "FCrDNS BROKEN: $PTR forward-resolves to '${FWD:-nothing}', not $IP"
  fi
  case "$PTR" in
    *dynamic*|*dhcp*|*pool*|*static.*|*.tmg.md|*broadband*|*dsl*|*cable*)
      wn "PTR looks generic/dynamic ($PTR) -- reads as 'not a real mail server'";;
  esac
fi

# --------------------------------------------------------------- HELO name
hd "HELO / EHLO name"
if [ "$HELO" != "${HELO%.test}" ] || [ "$HELO" != "${HELO%.local}" ] || [ "$HELO" != "${HELO%.invalid}" ]; then
  er "HELO '$HELO' is a lab name -- real MXes reject the session"
else
  HFWD=$(digq "$HELO" A | head -1)
  if [ "$HFWD" = "$IP" ]; then
    ok "HELO $HELO -> $IP (resolves to egress, good)"
  elif [ -n "$HFWD" ]; then
    wn "HELO $HELO -> $HFWD (does not match egress $IP)"
  else
    er "HELO $HELO does not resolve -- use a hostname that A-records to $IP"
  fi
fi

# --------------------------------------------------------------- SPF
hd "SPF (domain: $DOMAIN)"
SPF=$(digq "$DOMAIN" TXT | tr -d '"' | grep -i 'v=spf1' | head -1)
if [ -z "$SPF" ]; then
  er "no SPF record on $DOMAIN -- add: v=spf1 ip4:$IP -all"
else
  ok "SPF present: $SPF"
  if echo "$SPF" | grep -qF "$IP"; then
    ok "SPF lists egress IP $IP literally"
  else
    wn "egress $IP not literally in SPF (may be via include/a/mx -- verify it authorizes $IP)"
  fi
fi

# --------------------------------------------------------------- DMARC
hd "DMARC"
DMARC=$(digq "_dmarc.$DOMAIN" TXT | tr -d '"' | grep -i 'v=DMARC1' | head -1)
if [ -z "$DMARC" ]; then
  er "no DMARC on _dmarc.$DOMAIN -- add: v=DMARC1; p=none; rua=mailto:postmaster@$DOMAIN"
else
  ok "DMARC present: $DMARC"
fi

# --------------------------------------------------------------- DKIM
hd "DKIM"
if [ -z "$SELECTOR" ]; then
  wn "no selector given -- cannot check DKIM. Re-run with the selector: $0 $DOMAIN $HELO <selector>"
else
  DKIM=$(digq "${SELECTOR}._domainkey.$DOMAIN" TXT | tr -d '"' | grep -i 'p=' | head -1)
  if [ -z "$DKIM" ]; then
    er "no DKIM key at ${SELECTOR}._domainkey.$DOMAIN"
  else
    ok "DKIM key found at ${SELECTOR}._domainkey.$DOMAIN"
  fi
fi

# --------------------------------------------------------------- blocklists
hd "Blocklists (DNSBL)"
if [ "$IP" = "0.0.0.0" ]; then
  wn "skipped -- no usable IPv4 egress to look up"
else
  # These zones are IPv4-only in the form queried here. Reversing anything else
  # with awk -F. yields a name nobody publishes, the lookup returns nothing, and
  # the case below reads that as "not listed" -- a false clean.
  REV=$(echo "$IP" | awk -F. '{print $4"."$3"."$2"."$1}')
  for bl in zen.spamhaus.org b.barracudacentral.org bl.spamcop.net; do
    res=$(digq "$REV.$bl" A | head -1)
    case "$res" in
      "")            ok "$bl: not listed (or query blocked)";;
      127.255.255.*) wn "$bl: query refused (open/public resolver) -- check manually on the web";;
      127.*)         er "$bl: LISTED ($res)";;
      *)             wn "$bl: unexpected '$res'";;
    esac
  done
  wn "DNSBL over a public resolver is unreliable; confirm at https://check.spamhaus.org/results?query=$IP"
fi

# --------------------------------------------------------- live SMTP handshake
hd "Live SMTP handshake (Gmail)"
if [ "$open25" -eq 0 ]; then
  wn "skipped -- port 25 is blocked"
elif ! command -v nc >/dev/null 2>&1; then
  wn "skipped -- 'nc' not installed"
else
  probe="ratecheck-preflight-$RANDOM@gmail.com"
  out=$({ printf 'EHLO %s\r\n' "$HELO"; sleep 2; \
          printf 'MAIL FROM:<postmaster@%s>\r\n' "$DOMAIN"; sleep 1; \
          printf 'RCPT TO:<%s>\r\n' "$probe"; sleep 1; \
          printf 'QUIT\r\n'; sleep 1; } \
        | timeout 25 nc -4 -w 8 gmail-smtp-in.l.google.com 25 2>/dev/null)
  banner=$(echo "$out" | grep -m1 '^220')
  # The RCPT reply is the LAST final-form line before the QUIT acknowledgement.
  # grep -m1 '^250' would match "250-mx.google.com at your service" -- the first
  # continuation line of the EHLO greeting -- and report a 250 whatever the
  # server actually said about the recipient, including a 5.7.x block of us.
  # A final-form reply has a space after the code; a continuation has a hyphen.
  rcpt=$(echo "$out" | grep -E '^[245][0-9][0-9] ' | grep -v '^221 ' | tail -1)
  if [ -z "$banner" ]; then
    er "no 220 banner -- connection refused/tarpitted (IP reputation or block)"
  else
    ok "banner: $banner"
    case "$rcpt" in
      550*) ok "RCPT known-bad -> $rcpt  (session accepted end-to-end)";;
      421*|4*) wn "RCPT -> $rcpt  (throttled/tempfail -- IP may be rate-limited already)";;
      250*) wn "RCPT known-bad -> 250  (Gmail accepted a bogus address? unexpected)";;
      *) wn "RCPT reply: ${rcpt:-<none>}";;
    esac
  fi
fi

# --------------------------------------------------------------- verdict
hd "Verdict"
printf '  PASS=%d  WARN=%d  FAIL=%d\n\n' "$pass" "$warn" "$fail"
if [ "$fail" -gt 0 ]; then
  echo "  NO-GO: fix every [FAIL] above before the first run."
  echo "  A blocked :25, broken FCrDNS, or missing SPF means real MXes reject you"
  echo "  on identity, not on rate -- and the run measures nothing."
  exit 1
elif [ "$warn" -gt 0 ]; then
  echo "  GO WITH CAUTION: no hard blockers, but review each [WARN]."
  echo "  Start with Gmail only, low rate, and watch for policy replies."
else
  echo "  GO: sender identity looks clean. Suggested verify identity flags:"
  echo "    -helo $HELO -mail-from verify@$DOMAIN"
fi
