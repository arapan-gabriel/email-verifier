package prober

import (
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
)

// Only a class that means "come back later" carries a hint. Attaching one to an
// answer would invite the caller to re-ask a question already answered.
func TestRetryHintOnlyForDeferrals(t *testing.T) {
	for class, want := range map[Class]bool{
		ClassDeferred:    true,
		ClassThrottled:   true,
		ClassNoBudget:    true,
		ClassPaused:      true,
		ClassValid:       false,
		ClassInvalid:     false,
		ClassPolicy:      false,
		ClassGuarded:     false,
		ClassBadSequence: false,
		ClassConnError:   false,
	} {
		got := retryHint(class, "450 4.2.0 Greylisted", 15*time.Minute)
		if (got > 0) != want {
			t.Errorf("%s: hint = %d, want present=%v", class, got, want)
		}
	}
}

func TestRetryHintReadsAnExplicitServerHint(t *testing.T) {
	for reply, want := range map[string]int{
		"450 4.2.0 Greylisted, try again in 5 minutes":     300,
		"451 4.7.1 Please retry in 120 seconds":            120,
		"450 Deferred: come back in 2 hours":               7200,
		"450 4.7.1 Greylisting in effect, try again later": 900, // no number → default
		"450 4.2.0 Temporary failure":                      900,
	} {
		if got := retryHint(ClassDeferred, reply, 15*time.Minute); got != want {
			t.Errorf("retryHint(%q) = %d, want %d", reply, got, want)
		}
	}
}

// A server does not get to set our schedule.
func TestRetryHintIsClamped(t *testing.T) {
	if got := retryHint(ClassDeferred, "450 try again in 3 seconds", time.Minute); got != 60 {
		t.Errorf("hint = %d, want the 60s floor — retrying that soon burns a token for nothing", got)
	}
	if got := retryHint(ClassDeferred, "450 try again in 30 hours", time.Minute); got != int(maxRetryHint.Seconds()) {
		t.Errorf("hint = %d, want the ceiling — a live address must not be abandoned for a day", got)
	}
	if got := retryHint(ClassDeferred, "450 no hint", time.Second); got != 60 {
		t.Errorf("a too-small configured default was honoured: %d", got)
	}
}

// A paused MX is the one case where the answer is exact rather than a guess.
func TestRetryAfterForPausedIsExact(t *testing.T) {
	err := &pacer.PausedError{MXHost: "mx.test", Until: time.Now().Add(7 * time.Minute)}
	got := retryAfterFor(err, 0, "", 15*time.Minute)
	if got < 400 || got > 425 { // ~420s, allowing for scheduling
		t.Errorf("retry hint = %ds, want about 420 — the pacer knows exactly when", got)
	}
}

func TestRetryAfterForUnreachableBucketUsesTheDefault(t *testing.T) {
	if got := retryAfterFor(errNoBudget{}, 0, "", 5*time.Minute); got != 300 {
		t.Errorf("hint = %d, want the configured 300", got)
	}
}

type errNoBudget struct{}

func (errNoBudget) Error() string { return "connection refused" }

func TestParseHintRejectsNonsense(t *testing.T) {
	for _, reply := range []string{
		"450 4.2.0 Greylisted",
		"250 2.1.5 OK",
		"450 try again in 0 minutes",
		"450 message size 5 megabytes exceeded",
	} {
		if d, ok := parseHint(reply); ok {
			t.Errorf("parseHint(%q) = %s, want no match", reply, d)
		}
	}
}
