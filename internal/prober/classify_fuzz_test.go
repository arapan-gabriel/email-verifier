package prober

import (
	"bufio"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// Plan 027, fix 1 (invariant 1). A reply code is three ASCII digits whose first
// is 2–5. Anything else is a peer that does not speak SMTP, and a peer that
// does not speak SMTP may never condemn an address: "999 no such user" and
// "600 user unknown" used to class invalid.
func TestReadReplyRefusesCodesOutsideSMTP(t *testing.T) {
	for _, wire := range []string{
		"999 no such user\r\n",
		"600 user unknown\r\n",
		"-55 x\r\n",
		"+25 ok\r\n",
		"2xx ok\r\n",
		"199 informational\r\n",
		"099 no\r\n",
		" 25 ok\r\n",
		"550-first line\r\n999 last line is not smtp\r\n",
	} {
		if code, _, err := readReply(bufio.NewReader(strings.NewReader(wire))); err == nil {
			t.Errorf("readReply(%q) = %d, nil; want an error", wire, code)
		}
	}
	for _, wire := range []string{"250 ok\r\n", "599 edge\r\n", "200 edge\r\n", "550\r\n"} {
		if _, _, err := readReply(bufio.NewReader(strings.NewReader(wire))); err != nil {
			t.Errorf("readReply(%q): %v, want accepted", wire, err)
		}
	}
}

// Classify is exported, and a caller could hand it a code readReply never
// would. Outside 200–599 the answer is unknown, whatever the prose says.
func TestClassifyOutOfRangeIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		code int
		text string
	}{
		{999, "999 no such user"},
		{600, "600 user unknown"},
		{-55, "-55 x"},
		{25, "+25 ok"},
		{0, "user unknown"},
		{621, "621 greylisted"}, // found by FuzzClassify: the range check must precede the wording
		{199, "199 does not exist"},
		{1000, "1000 no such user"},
	} {
		if got := Classify(tc.code, tc.text); got != ClassUnknown {
			t.Errorf("Classify(%d, %q) = %s, want unknown", tc.code, tc.text, got)
		}
	}
}

// The dark branches of classifyPermanent, each pinned to who the reply is
// about.
func TestClassifyPermanentBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want Class
	}{
		{"subject 3, sender wording only", "550 5.3.0 client host rejected", ClassPolicy},
		{"subject 5, sender wording only", "550 5.5.0 helo command rejected", ClassPolicy},
		{"subject 6, sender wording only", "554 5.6.0 not authorized", ClassPolicy},
		{"subject 3, mailbox wording", "550 5.3.0 no such user", ClassInvalid},
		{"subject 5, no wording", "550 5.5.0 requested action not taken", ClassInvalid},
		{"subject 6, no wording", "554 5.6.0 message rejected", ClassInvalid},
		{"5.2.2 mailbox full is valid", "552 5.2.2 over quota", ClassValid},
		{"5.2.1 disabled is invalid", "550 5.2.1 mailbox disabled", ClassInvalid},
		{"subject 4, no mailbox wording", "550 5.4.1 relay denied", ClassPolicy},
		{"subject 7, no mailbox wording", "550 5.7.1 message refused", ClassPolicy},
		{"subject 7, mailbox wording", "550 5.7.1 recipient not found", ClassInvalid},
		{"subject 4, mailbox wording", "550 5.4.1 user unknown", ClassInvalid},
		{"subject 1 recipient", "550 5.1.1 bad destination", ClassInvalid},
		{"5.7.27 is sender", "550 5.7.27 null mx for sender", ClassPolicy},
		{"no code, mailbox wording", "550 mailbox not found", ClassInvalid},
		{"no code, sender wording", "550 client host rejected", ClassPolicy},
		{"no code, both kinds of wording", "550 user unknown, your mail from was rejected too", ClassInvalid},
		{"no code, no wording", "550 go away", ClassInvalid},
		{"unknown subject 9", "550 5.9.0 odd", ClassInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(550, tc.text); got != tc.want {
				t.Errorf("Classify(550, %q) = %s, want %s", tc.text, got, tc.want)
			}
		})
	}
}

func TestSubjectAndDetailRejectMalformedCodes(t *testing.T) {
	for _, ec := range []string{"", "5", "5.1", "5.x.1", "5.1.1.1"} {
		if subject(ec) != -1 && strings.Count(ec, ".") != 2 {
			t.Errorf("subject(%q) = %d, want -1", ec, subject(ec))
		}
	}
	if subject("5.x.1") != -1 || detail("5.1.x") != -1 || detail("5.1") != -1 {
		t.Error("a non-numeric part must give -1")
	}
}

func FuzzReadReply(f *testing.F) {
	for _, s := range []string{
		"250 ok\r\n", "550-a\r\n550 b\r\n", "999 no such user\r\n", "600 user unknown\r\n",
		"-55 x\r\n", "+25 ok\r\n", "2xx\r\n", "25\r\n", "", "\r\n", "421-\r\n421 4.7.0 x\r\n",
		strings.ReplaceAll(hetznerRBL, "\n", "\r\n") + "\r\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		code, text, err := readReply(bufio.NewReader(strings.NewReader(wire)))
		if err != nil {
			return
		}
		if code < 200 || code > 599 {
			t.Fatalf("readReply(%q) accepted code %d", wire, code)
		}
		// Bounded: whatever was kept stops growing once maxReplyBytes is
		// reached, so at most one further line past the bound survives.
		lines := strings.Split(text, "\n")
		longest := 0
		for _, l := range lines {
			longest = max(longest, len(l))
		}
		if len(text) > maxReplyBytes+longest+1 {
			t.Fatalf("kept %d bytes, bound is %d plus one line", len(text), maxReplyBytes)
		}
	})
}

// FuzzClassify is invariant 1 as a property, over every code a caller could
// pass, not just the ones readReply admits.
func FuzzClassify(f *testing.F) {
	for _, s := range []struct {
		code int
		text string
	}{
		{250, "250 ok"}, {550, "550 5.1.1 user unknown"}, {450, "450 4.2.0 greylisted"},
		{421, "421 too many"}, {999, "999 no such user"}, {600, "600 user unknown"},
		{554, "554 5.1.8 sender address rejected"}, {451, "451 4.7.650 rate limit exceeded greylist"},
		{550, hetznerRBL}, {-55, "-55 x"}, {503, "503 bad sequence"},
	} {
		f.Add(s.code, s.text)
	}
	f.Fuzz(func(t *testing.T, code int, text string) {
		got := Classify(code, text)
		if got == ClassInvalid && (code < 500 || code > 599) {
			t.Fatalf("Classify(%d, %q) = invalid outside 5xx (invariant 1)", code, text)
		}
		if code >= 400 && code < 500 && (got == ClassValid || got == ClassInvalid) {
			t.Fatalf("Classify(%d, %q) = %s: a 4xx is never a mailbox verdict", code, text, got)
		}
		lower := strings.ToLower(text)
		if (strings.Contains(lower, "greylist") || strings.Contains(lower, "grey list")) && got == ClassThrottled {
			t.Fatalf("Classify(%d, %q) = throttled on greylist wording", code, text)
		}
		if senderCodes[EnhancedCode(lower)] && got == ClassInvalid {
			t.Fatalf("Classify(%d, %q) = invalid on a sender-only code", code, text)
		}
		if (code < 200 || code > 599) && got != ClassUnknown {
			t.Fatalf("Classify(%d, %q) = %s, want unknown outside SMTP's codes", code, text, got)
		}
	})
}

var enhancedShape = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}$`)

func FuzzEnhancedCode(f *testing.F) {
	for _, s := range []string{"550 5.1.1 x", "250-2.1.0 OK", "550 no", "", "5.1.1", "450 4.999.999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, reply string) {
		ec := EnhancedCode(reply)
		if ec != "" && !enhancedShape.MatchString(ec) {
			t.Fatalf("EnhancedCode(%q) = %q, not an RFC 3463 code", reply, ec)
		}
		_ = subject(reply)
		_ = detail(reply)
		_ = subject(ec)
		_ = detail(ec)
	})
}

func FuzzParseHint(f *testing.F) {
	for _, s := range []string{
		"450 try again in 5 minutes", "451 retry after 30 seconds", "450 wait 2 hours",
		"450 no hint", "", "450 in 99999999999999999999 hours", "450 in -3 minutes",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		d, ok := parseHint(text)
		if ok && d <= 0 {
			t.Fatalf("parseHint(%q) = %v, true; want a positive duration", text, d)
		}
		c := clampHint(d)
		if c < minRetryHint || c > maxRetryHint {
			t.Fatalf("clampHint(%v) = %v, outside [%v, %v]", d, c, minRetryHint, maxRetryHint)
		}
	})
}

func FuzzRedactReply(f *testing.F) {
	for _, s := range []string{
		"550 5.1.1 <john@example.com> user unknown", "550 \"a b\"@example.com no", "250 ok",
		"", "@@@@", strings.Repeat("x@y ", 400), "550 j\u00f6hn@ex\u00e4mple.com unknown",
	} {
		f.Add(s, DefaultReplyMaxChars)
	}
	f.Fuzz(func(t *testing.T, text string, limit int) {
		out := redactReply(text, limit)
		if strings.Contains(out, "@") {
			t.Fatalf("redactReply(%q) = %q still carries an @", text, out)
		}
		if limit > 0 && utf8.RuneCountInString(out) > limit+1 {
			t.Fatalf("redactReply output is %d runes, bound %d plus the marker", utf8.RuneCountInString(out), limit)
		}
	})
}
