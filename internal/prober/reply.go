package prober

import "strings"

// ReplyEvent is what a server said, in a form safe to log.
//
// The prober hands this to Options.OnReply already redacted, so no wiring
// mistake in a caller can put a recipient into a log line. Plan 009 asserts
// that no address appears at info level, and the reply is the one field where
// one can arrive through the server's mouth rather than ours.
type ReplyEvent struct {
	MXHost       string
	Class        Class
	SMTPCode     int
	EnhancedCode string
	// Reply is redacted and truncated. Empty when the class is one of our own
	// refusals, in which case Err carries the reason.
	Reply string
	Err   string
}

// DefaultReplyMaxChars caps a logged reply. Long enough for the enhanced code
// and the sentence that follows it, which is where the meaning is.
const DefaultReplyMaxChars = 200

// redactReply removes anything address-shaped from a server's reply and caps
// its length.
//
// Strip BEFORE truncating. The other order reads as equivalent and is not:
// cutting "550 5.1.1 <john.smith@example.com> unknown" at 26 characters leaves
// "550 5.1.1 <john.smith@examp", which no longer looks like an address to any
// matcher and still carries the whole local part.
//
// A token is redacted whole rather than pattern-matched, because a mangled or
// unusual address must be removed too — the goal is that nothing with an "@"
// in it survives, not that well-formed addresses are recognised.
func redactReply(s string, limit int) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, f := range strings.Fields(s) {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.ContainsRune(f, '@') {
			b.WriteString("<redacted>")
			continue
		}
		b.WriteString(f)
	}
	out := b.String()
	if limit <= 0 {
		return out
	}
	// Count runes, not bytes: a server replying in UTF-8 must not have a
	// character cut in half and turned into a replacement rune in the log.
	if r := []rune(out); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return out
}

// explains reports whether a class needs a reply logged to be understood.
//
// valid and invalid speak for themselves — a 250 or a clean 5.1.x is the whole
// story, and they are the common case, so logging them would bury the rest.
// Everything else is a verdict somebody will later have to explain: a block
// that moved the warm-up ladder's stop rule, a throttle that halved a rate, a
// deferral that scheduled a retry.
func (c Class) explains() bool {
	switch c {
	case ClassValid, ClassInvalid:
		return false
	}
	return true
}
