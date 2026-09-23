package relay

import (
	"strings"
	"testing"
	"time"
)

// FuzzMessageBuild: whatever a caller sends, no header name or value that
// Build returns carries a CR or LF — header injection (plan 027). A message
// that Build accepts renders to exactly one header block: the first blank line
// is the one Render writes.
func FuzzMessageBuild(f *testing.F) {
	for _, s := range []struct{ from, to, subject, text, html, name, value string }{
		{"a@x.test", "b@y.test", "hi", "body", "", "X-Tag", "v"},
		{"a@x.test", "b@y.test", "hi\r\nBcc: c@z.test", "body", "", "", ""},
		{"a@x.test", "b@y.test", "hi", "", "<p>x</p>", "X-A\r\nBcc", "c@z.test"},
		{"a@x.test", "b@y.test", "hi", "t", "h", "X-A", "v\nBcc: c@z.test"},
		{"Name <a@x.test>", "b@y.test", "héllo wörld", "t", "", "From", "evil@z.test"},
		{"a@x.test\r\nBcc: c@z.test", "b@y.test", "hi", "t", "", "", ""},
	} {
		f.Add(s.from, s.to, s.subject, s.text, s.html, s.name, s.value)
	}
	f.Fuzz(func(t *testing.T, from, to, subject, text, html, name, value string) {
		m := Message{From: from, To: to, Subject: subject, Text: text, HTML: html}
		if name != "" || value != "" {
			m.Extra = []Header{{Name: name, Value: value}}
		}
		headers, body, err := m.Build(time.Unix(1_700_000_000, 0), "id@x.test")
		if err != nil {
			return
		}
		if reservedHeaders[strings.ToLower(strings.TrimSpace(name))] && len(m.Extra) > 0 {
			t.Fatalf("caller-supplied reserved header %q was accepted", name)
		}
		for _, h := range headers {
			if strings.ContainsAny(h.Name, "\r\n") || strings.ContainsAny(h.Value, "\r\n") {
				t.Fatalf("header %q: %q carries CR or LF", h.Name, h.Value)
			}
		}
		wire := string(Render(headers, body))
		head, _, found := strings.Cut(wire, "\r\n\r\n")
		if !found {
			t.Fatal("rendered message has no header/body separator")
		}
		if got, want := strings.Count(head, "\r\n")+1, len(headers); got != want {
			t.Fatalf("header block has %d lines for %d headers:\n%s", got, want, head)
		}
	})
}
