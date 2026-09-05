package prober

import "testing"

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
