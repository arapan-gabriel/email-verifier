# Service — outbound relay (`internal/relay`) — Phase C

Send transactional mail from the isolated, warmed IP. Detailed by plans 014–015.

- DKIM signing; SPF/DMARC alignment on the sending domain.
- Send queue reusing the per-MX pacer and IP-health from Phase A/B.
- Bounce/complaint capture feeds IP health and suppression (hard bounce → suppress).
- Authenticated `POST /send`, separate code path from verification (never mixes with probing).
