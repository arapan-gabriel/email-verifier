# Service — mail relay

Outbound transactional mail from the isolated IP (plan 014). Built; **off in the shipped config**,
and it stays off until the warm-up ladder finishes and plan 015 can read a bounce.

## Why it is here at all

The same reason verification is: reputation earned by one activity is spent by the other, and
neither belongs on the product host. Data Scout sends password resets today through a personal
Gmail account — this replaces that, with a domain that is signed, aligned and watched.

## The shape

```
POST /send ──▶ assemble ──▶ DKIM-sign ──▶ Redis (AOF) ──▶ drain ──▶ pacer ──▶ SMTP+STARTTLS
   202                                        │                                    │
   "durable", not "delivered"                 └── survives a restart               └── bounce → plan 015
```

- **`202` means durable, not delivered.** A caller told `202` may stop holding the message, so the
  route does not answer until a restart could not lose it. What a receiving server eventually says
  arrives as a bounce, never as that response.
- **One queue, on Redis with AOF.** A verification that is lost can be asked again; a message
  accepted from a caller and then dropped cannot. The body is written before the id is queued, a
  retry is removed from the schedule before it is made ready, and a message that has had its
  attempts is buried rather than deleted — so "we sent it" and "we gave up" stay distinguishable.
- **One pacer, shared with verification.** The receiving server sees one IP and does not care which
  of our two activities it is, so a busy MX slows both and a refused budget defers rather than
  sending unpaced.
- **Separate code path from the prober.** Verification never sends `DATA` (invariant 8) and this
  package only sends `DATA`. They share the pacer and the limiter and nothing else, which is what
  makes that statement checkable rather than a promise.

## What stops a send

Both of these **fail closed**, which is the difference from the verify path. Verification has an
authoritative check upstream and this has nothing between the queue and the socket.

| Condition | Behaviour |
|---|---|
| The recipient is suppressed | The message is dropped, not retried. Checked at accept **and again at send** — a message can wait hours, and an erasure that arrives meanwhile must win |
| The suppression list is stale or unreadable | Nothing is sent at all. `503` to the caller |
| The sending IP is listed | Sending stops; **verification continues**, because asking questions does not make a listing worse and every message sent from a listed address does |
| No budget | Deferred, never sent unpaced (invariant 5) |

## Signing

DKIM `relaxed/relaxed`, RSA-SHA256, with the key at `/etc/verifierd/dkim/s1.private` and the
selector published at `s1._domainkey.<domain>`. The signature covers `From`, `To`, `Subject`,
`Date`, `Message-ID`, `MIME-Version` and `Content-Type`: a relay that could rewrite any of those
while keeping the signature valid would make the signature worthless.

A caller may not supply `From`, `Date`, `Message-ID` or any other header this service sets — a
caller that could choose `From` could send as anyone this domain can sign for. A CR or LF in a
header value is refused rather than escaped.

**Enabling the relay without a signing key, a domain or a return path refuses to boot.** Each is a
way to send mail that fails authentication silently, which is worse than not sending: the message
arrives, lands in spam, and teaches the receiver that this IP sends unsigned mail.

## Transport

`STARTTLS` when offered, **unverified**. Almost no MX presents a certificate matching the name we
looked up, so requiring validity would send everything in the clear instead. This buys
confidentiality against a passive observer, not proof of who is listening.

Every line of a reply is read, not only the last: `STARTTLS` is announced on a continuation line of
the `EHLO` response, and reading only the last would silently send every message unencrypted.

IPv4 only, like verification (invariant 3) — the published identity covers the IPv4 address alone.

## Retries

A `5xx` is an answer and is not retried into the ground. A `4xx` is a schedule: exponential backoff
from `relay.retry_base`, bounded by `relay.max_attempts`, then buried. A server that has refused
five times will not take the sixth, and continuing to offer it spends the IP's standing on one
address.

## What is not here

The **return path**. VERP puts a token in the envelope sender so a bounce identifies its message
without the body being parsed, and `relay.return_path_domain` carries it — but nothing reads that
mailbox until plan 015. Until it does, enabling the relay would send mail whose failures nobody
sees, which is how a warmed IP is spent.
