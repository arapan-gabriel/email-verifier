package prober

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/pacer"
)

// Retry hints are clamped. A server saying "try again in 3 seconds" would have
// us burn a token before its greylist window opens; one saying "in 30 days"
// would have the caller abandon a live address. Neither gets to set our
// schedule.
const (
	minRetryHint = 60 * time.Second
	maxRetryHint = 6 * time.Hour
)

// explicitHint matches the handful of shapes providers actually use to say when
// to come back. The parse is deliberately narrow: reading intent out of SMTP
// prose is guesswork, and the configured default is a perfectly good answer.
var explicitHint = regexp.MustCompile(
	`(?i)(?:try again|retry|come back)(?:\s+\w+){0,3}?\s+(\d{1,5})\s*(seconds?|secs?|minutes?|mins?|hours?|hrs?|s|m|h)\b`)

// retryHint returns the seconds to wait before asking again, or 0 when the
// class is an answer rather than a deferral.
//
// Only classes that mean "come back later" carry a hint. A `valid` or `invalid`
// is a result; attaching a retry to it would invite the caller to re-ask a
// question that has been answered.
func retryHint(class Class, reply string, fallback time.Duration) int {
	switch class {
	case ClassDeferred, ClassThrottled, ClassNoBudget, ClassPaused:
	default:
		return 0
	}
	if d, ok := parseHint(reply); ok {
		return int(clampHint(d).Seconds())
	}
	return int(clampHint(fallback).Seconds())
}

// retryAfterFor builds the hint for a refusal that never reached the server. A
// paused MX is the one case where the answer is exact rather than a guess.
func retryAfterFor(err error, _ int, reply string, fallback time.Duration) int {
	var paused *pacer.PausedError
	if errors.As(err, &paused) {
		return int(clampHint(paused.RetryAfter()).Seconds())
	}
	return retryHint(ClassNoBudget, reply, fallback)
}

// applyRetryAfter stamps one hint across a whole chunk's results.
func applyRetryAfter(results map[string]Result, seconds int) {
	if seconds <= 0 {
		return
	}
	for k, r := range results {
		r.RetryAfterSeconds = seconds
		results[k] = r
	}
}

func parseHint(reply string) (time.Duration, bool) {
	m := explicitHint.FindStringSubmatch(reply)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	var unit time.Duration
	switch strings.TrimSuffix(strings.ToLower(m[2]), "s") {
	case "second", "sec", "":
		unit = time.Second
	case "minute", "min", "m":
		unit = time.Minute
	case "hour", "hr", "h":
		unit = time.Hour
	default:
		return 0, false
	}
	return time.Duration(n) * unit, true
}

func clampHint(d time.Duration) time.Duration {
	return min(maxRetryHint, max(minRetryHint, d))
}
