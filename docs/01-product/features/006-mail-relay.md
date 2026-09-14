# Feature 006 — mail relay

Transactional mail (password resets, invitations, notifications) leaves from the isolated sending
IP rather than from the product host or a personal mailbox.

**Status:** built, off. Plan 014 shipped the service and plan 015 closed the return path (a
Cloudflare Email Worker posts every bounce to Data Scout, which records a hard bounce as an
`invalid` verdict and sends a complaint back here as an IP-health penalty). It stays disabled
until the probe IP's warm-up ladder finishes.

**Why it matters to a customer:** a reset email that lands in spam is an account they cannot get
back into. Sending from a domain that is signed (DKIM), aligned (SPF, DMARC) and warmed is what
makes the difference — and none of it is available from a residential connection or a personal
Gmail account, which is what sends those messages today.

**What it does not do yet:** send. The failure-reading half is done — a bounce is matched to the
message that earned it through the VERP token, so a DSN that names no recipient is still
actionable, and a complaint pauses sending without touching verification. What remains is the
warm-up ladder finishing, which is a calendar, not a code gap.
