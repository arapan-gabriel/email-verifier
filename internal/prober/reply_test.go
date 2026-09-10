package prober

import (
	"strings"
	"testing"
)

// The reply is the one field where an address can reach a log line through the
// server's mouth rather than ours, so this is the test plan 009's "no address
// at info level" rule actually rests on once replies are logged.
func TestRedactReplyRemovesEveryAddressShape(t *testing.T) {
	for _, in := range []string{
		"550 5.1.1 <john.smith@example.com>: Recipient address rejected",
		"550 5.1.1 john.smith@example.com unknown",
		"552 5.2.2 mailbox john.smith@example.com is over quota,",
		"553 recipient=john.smith@example.com rejected",
		"550 broken@@example..com",
	} {
		got := redactReply(in, 0)
		if strings.Contains(got, "@") {
			t.Errorf("an @ survived redaction:\n  in:  %q\n  out: %q", in, got)
		}
		if strings.Contains(got, "john.smith") {
			t.Errorf("the local part survived redaction:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// Truncating first and stripping second looks equivalent and is not: the cut
// destroys the shape that redaction matches on while leaving the local part
// behind. This asserts the order, not just the outcome.
func TestRedactReplyStripsBeforeTruncating(t *testing.T) {
	const in = "550 5.1.1 <john.smith@example.com> unknown"
	got := redactReply(in, 26)
	if strings.Contains(got, "john") {
		t.Fatalf("truncation exposed the local part: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("expected the address to be replaced before the cut: %q", got)
	}
}

func TestRedactReplyCapsLengthInRunes(t *testing.T) {
	got := redactReply(strings.Repeat("ы", 50), 10)
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != 10 {
		t.Fatalf("capped to %d runes, want 10 (%q)", n, got)
	}
	if strings.Contains(got, "�") {
		t.Fatalf("a multi-byte character was cut in half: %q", got)
	}
}

func TestRedactReplyLeavesOrdinaryTextAlone(t *testing.T) {
	const in = "452 4.5.3 Too many recipients"
	if got := redactReply(in, 0); got != in {
		t.Fatalf("redactReply(%q) = %q, want it unchanged", in, got)
	}
}

// valid and invalid are the common case and speak for themselves; logging them
// would bury the verdicts that need explaining.
func TestOnlyHardToReadClassesAreExplained(t *testing.T) {
	quiet := []Class{ClassValid, ClassInvalid}
	loud := []Class{ClassPolicy, ClassThrottled, ClassDeferred, ClassTimeout,
		ClassConnError, ClassNoBudget, ClassPaused, ClassIPBurned, ClassUnknown}
	for _, c := range quiet {
		if c.explains() {
			t.Errorf("class %q should not be logged", c)
		}
	}
	for _, c := range loud {
		if !c.explains() {
			t.Errorf("class %q should be logged", c)
		}
	}
}

func TestExplainHookFiresRedactedAndOnlyWhenNeeded(t *testing.T) {
	var got []ReplyEvent
	p := New(Options{
		OnReply: func(ev ReplyEvent) { got = append(got, ev) },
	})
	p.record("mx.example.com", Result{
		Class: ClassPolicy, SMTPCode: 550, EnhancedCode: "5.4.1",
		Reply: "550 5.4.1 <john.smith@example.com>: Access denied",
	})
	p.record("mx.example.com", Result{Class: ClassValid, SMTPCode: 250, Reply: "250 2.1.5 OK"})

	if len(got) != 1 {
		t.Fatalf("hook fired %d times, want 1 (valid must not be logged)", len(got))
	}
	ev := got[0]
	if ev.MXHost != "mx.example.com" || ev.SMTPCode != 550 || ev.EnhancedCode != "5.4.1" {
		t.Fatalf("event lost its context: %+v", ev)
	}
	if strings.Contains(ev.Reply, "@") || strings.Contains(ev.Reply, "john") {
		t.Fatalf("the hook handed out an un-redacted reply: %q", ev.Reply)
	}
}

// A nil hook is the off switch, and it must not cost a call or a panic.
func TestExplainWithNoHookIsSilent(_ *testing.T) {
	New(Options{}).record("mx.example.com", Result{Class: ClassPolicy, Reply: "550 5.7.1 blocked"})
}
