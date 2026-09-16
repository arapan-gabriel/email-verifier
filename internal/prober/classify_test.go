package prober

import (
	"bufio"
	"strings"
	"testing"
)

// The table that matters most in this repo: which replies may condemn a
// mailbox and which may not (invariant 1,
// docs/03-engineering/patterns/smtp-classification.md).
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		text string
		want Class
	}{
		{"accepted", 250, "250 2.1.5 OK", ClassValid},
		{"no such user", 550, "550 5.1.1 The email account that you tried to reach does not exist", ClassInvalid},
		{"user unknown, no enhanced code", 550, "550 User unknown", ClassInvalid},
		// A full mailbox is proof the mailbox exists.
		{"mailbox full", 550, "550 5.2.2 The recipient mailbox is over quota", ClassValid},
		// Over quota is about the recipient's box, not about our rate.
		{"over quota 4.2.2", 452, "452 4.2.2 The email account is over quota", ClassDeferred},
		{"greylisted", 450, "450 4.2.0 Greylisted, please try again later", ClassDeferred},
		{"grey list spelled out", 451, "451 grey list in effect", ClassDeferred},
		{"outlook rate 4.7.650", 451, "451 4.7.650 The mail server has exceeded the maximum rate", ClassThrottled},
		{"421 unusual rate", 421, "421 4.7.0 Our system has detected an unusual rate of traffic from your IP", ClassThrottled},
		// Everything below is about our IP, never about the mailbox.
		{"reverse dns 5.7.25", 550, "550 5.7.25 Forward-confirmed reverse DNS failed", ClassPolicy},
		{"spf 5.7.23", 550, "550 5.7.23 SPF check failed", ClassPolicy},
		{"blocked using spamhaus", 554, "554 5.7.1 Service unavailable; Client host blocked using Spamhaus", ClassPolicy},
		{"access denied", 550, "550 5.4.1 Recipient address rejected: Access denied", ClassPolicy},
		{"bad sequence is our bug", 503, "503 5.5.1 Bad sequence of commands", ClassBadSequence},
		// Found on the first real run (2026-08-28). RFC 3463 puts these in the
		// "addressing" subject next to the recipient codes, but they are about
		// the SENDER: reading them as "no such recipient" condemns every
		// address in the batch. ../ds-smtp-retry still carries this bug.
		{"bad sender system address", 554, "554 5.1.8 <verify@probe.example.test>: Sender address rejected: Domain not found", ClassPolicy},
		{"bad sender mailbox syntax", 501, "501 5.1.7 Bad sender address syntax", ClassPolicy},
		// Warm-up ladder day 5 (2026-09-15): a receiving server's own blocklist,
		// no enhanced code, and its reason only in the continuation lines.
		{"hetzner rbl, read whole", 550, hetznerRBL, ClassPolicy},
		{"dnsbl named in prose", 550, "550 Rejected because 192.0.2.1 is in dnsbl.example.test", ClassPolicy},
		// Warm-up ladder day 6 (2026-09-16), plan 024: a refusal of the session
		// for want of encryption, carrying no enhanced code. The prober has no
		// STARTTLS step, so this says everything about us and nothing about the
		// mailbox — and it used to be recorded as `invalid`.
		{"tls demanded, no enhanced code", 550, "550 A TLS connection is required", ClassPolicy},
		{"rfc 3207 wording", 530, "530 Must issue a STARTTLS command first", ClassPolicy},
		{"session encryption demanded", 550, "550 Session encryption is required", ClassPolicy},
		// Unchanged: these already carried a sender-shaped code.
		{"starttls mandatory 5.7.0", 530, "530 5.7.0 STARTTLS is mandatory", ClassPolicy},
		// A reply that names both stays about the recipient — the existing rule,
		// asserted here because the TLS wording is new.
		{"tls wording but the user is the problem", 550, "550 5.1.1 No such user (TLS required for other mail)", ClassInvalid},
		// "rbl." must not fire on a word that merely contains the letters.
		{"marble is not an rbl", 550, "550 5.1.1 <info@marble.example>: user unknown", ClassInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.code, tc.text); got != tc.want {
				t.Errorf("Classify(%d, %q) = %s, want %s", tc.code, tc.text, got, tc.want)
			}
		})
	}
}

// Only a genuine rate signal may move the pacer. Deferrals are per-recipient
// and rate-independent; a policy block is about our IP, and slowing down does
// not grow a PTR record (invariants 5 and 6).
func TestIsThrottleExcludesDeferralsAndPolicy(t *testing.T) {
	throttles := map[Class]bool{ClassThrottled: true, ClassTimeout: true, ClassConnError: true}
	for _, c := range []Class{
		ClassValid, ClassInvalid, ClassDeferred, ClassThrottled, ClassTimeout,
		ClassConnError, ClassBadSequence, ClassPolicy, ClassUnknown,
	} {
		if got := c.IsThrottle(); got != throttles[c] {
			t.Errorf("%s.IsThrottle() = %v, want %v", c, got, throttles[c])
		}
	}
}

func TestIsTemp(t *testing.T) {
	temps := map[Class]bool{
		ClassDeferred: true, ClassThrottled: true, ClassTimeout: true,
		ClassConnError: true, ClassPolicy: true,
	}
	for _, c := range []Class{
		ClassValid, ClassInvalid, ClassDeferred, ClassThrottled, ClassTimeout,
		ClassConnError, ClassBadSequence, ClassPolicy, ClassUnknown,
	} {
		if got := c.IsTemp(); got != temps[c] {
			t.Errorf("%s.IsTemp() = %v, want %v", c, got, temps[c])
		}
	}
}

func TestEnhancedCodeReadsOnlyTheLeadingToken(t *testing.T) {
	for _, tc := range []struct{ reply, want string }{
		{"550 5.1.1 does not exist", "5.1.1"},
		{"250-2.1.0 OK", "2.1.0"},
		// Google appends a message id and Microsoft a server name; matching a
		// dotted token from the free text reads the wrong subject entirely.
		{"250 2.1.5 OK ffacd0b85a97d-482f.94 - gsmtp", "2.1.5"},
		{"550 No such user", ""},
	} {
		if got := EnhancedCode(tc.reply); got != tc.want {
			t.Errorf("EnhancedCode(%q) = %q, want %q", tc.reply, got, tc.want)
		}
	}
}

// hetznerRBL is the refusal Hetzner's managed mail sent the probe IP on
// 2026-09-15, captured whole with swaks from the node.
const hetznerRBL = "550-Unfortunately we cannot currently accept your e-mail due to the amount of\n" +
	"550-spam we are receiving from your server. Please check\n" +
	"550-https://rbl.your-server.de/?ip=192.0.2.1 for further details or contact\n" +
	"550-your server provider. / Leider koennen wir Ihre E-Mails aufgrund der\n" +
	"550-Spamaufkommens von Ihrem Server momentan nicht annehmen. Weitere Details\n" +
	"550-können Sie unter https://rbl.your-server.de/?ip=192.0.2.1 bzw. von\n" +
	"550 Ihrem Serveranbieter erfahren."

// The reason of a multi-line reply is in its first lines, not its last. Keeping
// only the final line is how a blocklist refusal became "invalid".
func TestReadReplyKeepsEveryLine(t *testing.T) {
	wire := strings.ReplaceAll(hetznerRBL, "\n", "\r\n") + "\r\n" + "250 2.0.0 next reply\r\n"
	r := bufio.NewReader(strings.NewReader(wire))
	code, text, err := readReply(r)
	if err != nil {
		t.Fatalf("readReply: %v", err)
	}
	if code != 550 || text != hetznerRBL {
		t.Errorf("readReply = %d %q, want 550 and the reply whole", code, text)
	}
	if got := Classify(code, text); got != ClassPolicy {
		t.Errorf("Classify(whole reply) = %s, want policy", got)
	}
	// The reader must stop exactly at the reply's end, or the next command
	// reads this reply's tail as its own answer.
	if code, text, err = readReply(r); err != nil || code != 250 || text != "250 2.0.0 next reply" {
		t.Errorf("next readReply = %d %q %v, want the following reply intact", code, text, err)
	}
}

// A server may talk for as long as it likes; the session still ends on its
// final line, and only a bounded amount of the text is kept.
func TestReadReplyIsBounded(t *testing.T) {
	line := "451-" + strings.Repeat("x", 96) + "\r\n"
	wire := strings.Repeat(line, 500) + "451 4.3.0 done\r\n" + "250 ok\r\n"
	r := bufio.NewReader(strings.NewReader(wire))
	code, text, err := readReply(r)
	if err != nil {
		t.Fatalf("readReply: %v", err)
	}
	if code != 451 {
		t.Errorf("code = %d, want the final line's 451", code)
	}
	if len(text) > maxReplyBytes+len(line) {
		t.Errorf("kept %d bytes, want at most about %d", len(text), maxReplyBytes)
	}
	if code, _, err = readReply(r); err != nil || code != 250 {
		t.Errorf("next readReply = %d %v, want 250 — the long reply was not consumed to its end", code, err)
	}
}
