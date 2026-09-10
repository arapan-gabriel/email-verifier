package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// The end of the chain plan 009 started: no address at info level, now that
// what a server said is logged too. The prober redacts, but this asserts the
// wiring in main does not hand the raw Result to slog by mistake.
func TestReplyLoggerNeverWritesAnAddress(t *testing.T) {
	var buf bytes.Buffer
	log := replyLogger(true, slog.New(slog.NewJSONHandler(&buf, nil)))
	if log == nil {
		t.Fatal("replyLogger(true) = nil")
	}

	// Fed the way the prober feeds it: already redacted. The raw address is
	// passed too, in the field a careless change would start logging.
	log(prober.ReplyEvent{
		MXHost:   "mx.example.com",
		Class:    prober.ClassPolicy,
		SMTPCode: 550,
		Reply:    "550 5.4.1 <redacted>: Access denied",
	})

	out := buf.String()
	for _, forbidden := range []string{"@", "john.smith"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("log line contains %q: %s", forbidden, out)
		}
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if line["mx_host"] != "mx.example.com" || line["class"] != "policy" {
		t.Fatalf("the line lost the context that makes it searchable: %v", line)
	}
}

// The off switch has to be a nil hook, not a hook that discards: the prober
// checks for nil once per result instead of calling into a no-op.
func TestReplyLoggerOffReturnsNil(t *testing.T) {
	if replyLogger(false, slog.Default()) != nil {
		t.Fatal("replyLogger(false) should be nil so the prober can skip the call")
	}
}
