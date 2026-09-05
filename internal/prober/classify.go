// Package prober runs SMTP RCPT TO verification and classifies the answer.
//
// Classification is the whole game: a validator that cannot tell "this mailbox
// does not exist" from "you are going too fast" will either delete good
// addresses or hammer a provider until its IP is worthless.
//
// Everything in this file is ported verbatim from
// ../ds-smtp-retry/ratecheck/internal/prober. It is deliberately kept close to
// upstream so measurements made in the lab still describe this code. It is the
// single place a reply becomes a class (invariant 1,
// docs/03-engineering/patterns/smtp-classification.md); nothing else in this
// service may map a code to a meaning.
package prober

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Class is what one SMTP reply means. It answers "who is this about" before
// it answers "what happened", which is the distinction invariant 1 rests on.
type Class string

// The classes a reply can carry. Only ClassValid and ClassInvalid are
// statements about a mailbox; every other class is a statement about us, the
// connection, or the moment.
const (
	ClassValid       Class = "valid"        // 250: the mailbox answered
	ClassInvalid     Class = "invalid"      // 550 and friends: it does not exist
	ClassDeferred    Class = "deferred"     // 4xx that is not obviously throttling
	ClassThrottled   Class = "throttled"    // 421, or 4xx/5xx that says "slow down"
	ClassTimeout     Class = "timeout"      // no answer in time
	ClassConnError   Class = "conn_error"   // refused, reset, DNS failure
	ClassBadSequence Class = "bad_sequence" // 503: our bug, never theirs
	// ClassPolicy is a permanent rejection that is about the connecting
	// client -- its IP, its rDNS, its HELO, its authentication -- and not
	// about the recipient. "550 5.7.25 Forward-confirmed reverse DNS failed"
	// says nothing whatsoever about whether the mailbox exists.
	ClassPolicy Class = "policy"
	// ClassGuarded is our own refusal to open the socket at all: the MX host
	// resolved only to addresses the SSRF guard rejects (invariant 2). Never a
	// statement about the mailbox, and not retryable either — a domain pointing
	// its MX at 127.0.0.1 will still be doing so tomorrow.
	//
	// Added here, not ported: ../ds-smtp-retry connects to loopback on purpose,
	// which is the one lab behaviour this service inverts.
	ClassGuarded Class = "guarded"
	// ClassNoBudget is the fail-closed path (invariant 5): the shared token
	// bucket could not be consulted, so the probe was not sent. An unconfirmed
	// verdict is recoverable; a blocklist entry is not.
	ClassNoBudget Class = "no_budget"
	// ClassPaused is this MX standing down for its cooldown after being
	// throttled at the floor of its band. Normal operation, not an incident.
	ClassPaused Class = "paused"
	// ClassIPBurned is this node standing itself down: the sending IP is listed
	// somewhere that matters, so probing on would deepen the damage rather than
	// produce answers. Our refusal, never a verdict (invariant 1).
	ClassIPBurned Class = "ip_burned"
	// ClassSuppressed is an address somebody asked to be forgotten. Never
	// probed, never mailed (invariant 9). Our refusal, and the most deliberate
	// one: it is the only class that is a statement about the *request* rather
	// than about the network.
	ClassSuppressed Class = "suppressed"
	ClassUnknown    Class = "unknown"
)

// IsTemp reports whether the sample means "try again later" rather than
// "here is your answer".
func (c Class) IsTemp() bool {
	switch c {
	case ClassDeferred, ClassThrottled, ClassTimeout, ClassConnError, ClassPolicy,
		ClassNoBudget, ClassPaused, ClassIPBurned:
		return true
	}
	return false
}

// IsThrottle reports whether the sample is evidence that we are going too
// fast. Deferrals are excluded on purpose: greylisting is rate-independent,
// and treating it as throttling drives the calibrator to a rate of zero.
// ClassPolicy is excluded for exactly the same reason -- slowing down does not
// grow a PTR record -- and if it counted, one blocked IP would calibrate every
// provider to zero.
func (c Class) IsThrottle() bool {
	switch c {
	case ClassThrottled, ClassTimeout, ClassConnError:
		return true
	}
	return false
}

// throttleHints are the phrases providers use when the problem is that we are
// going too fast. They are a labelling aid only: the calibrator's primary
// signal is that the error rate rises with the request rate, which needs no
// keyword list.
var throttleHints = []string{
	"too many", "rate limit", "ratelimit", "throttl", "exceeded", "unusual",
	"deferred", "try again later", "slow down", "reputation", "4.7.650",
	"too much mail", "connection limit", "temporarily deferred", "blocked",
	"blacklist", "spamhaus", "not accepted from your ip",
}

// senderHints place the blame on the connecting client rather than on the
// recipient. Measured against real providers, these are the replies that a
// keyword-only classifier reads as "mailbox does not exist" and acts on by
// deleting a perfectly good address.
var senderHints = []string{
	"reverse dns", "rdns", "fcrdns", "forward-confirmed", "ptr record",
	"access denied", "your mail from", "client host", "helo command",
	"not authorized", "dkim", "dmarc", "spf check", "spf record",
	"i=ip", "blocked", "blacklist", "spamhaus", "reputation",
	"not accepted from your ip", "unsolicited", "bad reputation",
	"sender address", "sender verify", "sender rejected", "sender domain",
}

// mailboxHints say plainly that the recipient is the problem. They are what
// lets an ambiguous policy code (5.7.1 is used both ways) still resolve to
// "invalid" when the provider spelled it out.
var mailboxHints = []string{
	"no such user", "user unknown", "unknown user", "does not exist",
	"doesn't exist", "no such recipient", "recipient not found",
	"mailbox unavailable", "mailbox not found", "invalid recipient",
	"user not found", "address does not exist", "no mailbox",
	"undeliverable", "address rejected: user", "recipient rejected",
}

// senderCodes are RFC 3463 codes that can only ever be about the sender.
// 5.7.25 is reverse-DNS validation, 5.7.23/24/26 are SPF/DKIM/DMARC.
//
// 5.1.7 and 5.1.8 sit in the "addressing" subject alongside the recipient
// codes, but the RFC assigns them to the *sender*: "bad sender's mailbox
// address syntax" and "bad sender's system address". Without them here the
// subject-1 branch below reads them as "no such recipient" and condemns every
// address in the batch — measured against a real MX on 2026-08-28, where a
// 554 5.1.8 about our envelope sender marked two live mailboxes invalid.
// This is a deviation from ../ds-smtp-retry, which carries the same bug.
var senderCodes = map[string]bool{
	"5.1.7": true, "5.1.8": true,
	"5.7.23": true, "5.7.24": true, "5.7.25": true, "5.7.26": true,
	"5.7.27": true,
}

// enhancedRe matches the RFC 3463 status code where the RFC puts it: directly
// after the three-digit reply code. Providers scatter other dotted tokens
// through the free text -- Google appends a message id, Microsoft a server
// name -- and matching those instead reads the wrong subject entirely.
var enhancedRe = regexp.MustCompile(`^[245]\d{2}[ -]\s*([245]\.\d{1,3}\.\d{1,3})\b`)

// EnhancedCode returns the RFC 3463 status code leading a reply, or "".
func EnhancedCode(reply string) string {
	m := enhancedRe.FindStringSubmatch(strings.TrimSpace(reply))
	if m == nil {
		return ""
	}
	return m[1]
}

// subject is the middle number of an RFC 3463 code: 1 addressing, 2 mailbox,
// 3 mail system, 4 network/routing, 5 protocol, 6 media, 7 security/policy.
func subject(ec string) int {
	parts := strings.Split(ec, ".")
	if len(parts) != 3 {
		return -1
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return n
}

func detail(ec string) int {
	parts := strings.Split(ec, ".")
	if len(parts) != 3 {
		return -1
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return -1
	}
	return n
}

func hasAny(t string, hints []string) bool {
	for _, h := range hints {
		if strings.Contains(t, h) {
			return true
		}
	}
	return false
}

// Classify turns one SMTP reply into a Class. It is the single place in this
// service where a code becomes a meaning
// (docs/03-engineering/patterns/smtp-classification.md).
func Classify(code int, text string) Class {
	t := strings.ToLower(text)

	// Greylisting says "try again later" too, but it is per-recipient and
	// rate-independent, so it must not be read as throttling.
	if strings.Contains(t, "greylist") || strings.Contains(t, "grey list") {
		return ClassDeferred
	}

	switch {
	case code == 421:
		return ClassThrottled
	case code == 503:
		return ClassBadSequence
	case code >= 200 && code < 300:
		return ClassValid
	case code >= 400 && code < 500:
		// 4xx is already a "come back later", so the only question is whether
		// it is our rate. The enhanced code adds nothing here, and reading it
		// would demote Outlook's 451 4.7.650 -- which IS rate-driven -- out of
		// the throttle signal the calibrator ramps against.
		if hasAny(t, throttleHints) {
			return ClassThrottled
		}
		return ClassDeferred
	case code < 500:
		return ClassUnknown
	}

	return classifyPermanent(t)
}

// classifyPermanent decides who a 5xx is about. The enhanced status code is
// the machine-readable answer to exactly that question, so it is read before
// the prose: "550 5.4.1 Recipient address rejected: Access denied" is
// Microsoft refusing the *connection*, and treating it as a missing mailbox
// deletes every address at that domain.
func classifyPermanent(t string) Class {
	ec := EnhancedCode(t)
	sender := hasAny(t, senderHints)
	mailbox := hasAny(t, mailboxHints)

	// Codes that cannot be about the recipient, whatever the text says.
	if senderCodes[ec] {
		return ClassPolicy
	}
	// Explicit sender-side wording, with nothing pointing at the recipient.
	if sender && !mailbox {
		return ClassPolicy
	}

	switch subject(ec) {
	case 1: // addressing — but only the recipient details; 1.7/1.8 are the
		// sender's and were caught by senderCodes above.
		return ClassInvalid
	case 2: // mailbox status
		// 5.2.2 is "mailbox full", which means the mailbox exists.
		if detail(ec) == 2 {
			return ClassValid
		}
		return ClassInvalid
	case 4, 7: // routing and policy: never inherently about the mailbox
		if mailbox {
			return ClassInvalid
		}
		return ClassPolicy
	case 3, 5, 6: // mail system, protocol, media
		if mailbox {
			return ClassInvalid
		}
		if sender {
			return ClassPolicy
		}
		return ClassInvalid
	}

	// No enhanced code at all: fall back to the wording.
	if mailbox {
		return ClassInvalid
	}
	if sender {
		return ClassPolicy
	}
	return ClassInvalid
}

func classifyNetErr(err error) Class {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ClassConnError
	}
	return ClassConnError
}

func readReply(r *bufio.Reader) (int, string, error) {
	var last string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, line, fmt.Errorf("short reply %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, line, fmt.Errorf("bad reply %q", line)
		}
		last = line
		if len(line) > 3 && line[3] == '-' {
			continue
		}
		return code, last, nil
	}
}
