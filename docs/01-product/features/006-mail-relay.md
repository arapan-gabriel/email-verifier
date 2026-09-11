# Feature 006 — mail relay

Transactional mail (password resets, invitations, notifications) leaves from the isolated sending
IP rather than from the product host or a personal mailbox.

**Status:** built, off. Plan 014 shipped the service; it stays disabled until the probe IP's warm-up
finishes and plan 015 can read a bounce.

**Why it matters to a customer:** a reset email that lands in spam is an account they cannot get
back into. Sending from a domain that is signed (DKIM), aligned (SPF, DMARC) and warmed is what
makes the difference — and none of it is available from a residential connection or a personal
Gmail account, which is what sends those messages today.

**What it does not do yet:** read its own bounces. Until plan 015, a message that fails is a message
nobody learns from — which is why the feature is finished and still switched off.
