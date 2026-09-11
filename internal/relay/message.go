package relay

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
)

// Header is one header line, before folding.
type Header struct {
	Name  string
	Value string
}

// Message is what a caller hands to POST /send, after validation.
type Message struct {
	From    string
	To      string
	Subject string
	Text    string
	HTML    string
	// Headers the caller added. Anything this package sets itself is refused
	// rather than overridden — see Build.
	Extra []Header
}

// reservedHeaders may not come from the caller. Letting one through would let a
// request forge a Message-ID, backdate a message, or — worst — set its own
// From, which is the identity DKIM and DMARC align against. A caller that could
// choose From could send as anyone this domain can sign for.
var reservedHeaders = map[string]bool{
	"from": true, "to": true, "subject": true, "date": true,
	"message-id": true, "mime-version": true, "content-type": true,
	"content-transfer-encoding": true, "dkim-signature": true,
	"return-path": true, "received": true,
}

// Build assembles the message and returns its headers and body separately, so
// the signer can canonicalize each without re-parsing what was just written.
func (m Message) Build(now time.Time, messageID string) ([]Header, []byte, error) {
	if err := m.validate(); err != nil {
		return nil, nil, err
	}

	headers := []Header{
		{"From", m.From},
		{"To", m.To},
		{"Subject", encodeSubject(m.Subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", "<" + messageID + ">"},
		{"MIME-Version", "1.0"},
	}

	var body []byte
	switch {
	case m.HTML != "" && m.Text != "":
		boundary, err := randomToken(16)
		if err != nil {
			return nil, nil, err
		}
		headers = append(headers,
			Header{"Content-Type", `multipart/alternative; boundary="` + boundary + `"`})
		// Plain text first: a reader that takes the last part it understands
		// should land on the HTML, and one that takes the first should land on
		// something readable. RFC 2046 orders parts least-to-most faithful.
		body = []byte(
			"--" + boundary + "\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
				normaliseNewlines(m.Text) + "\r\n" +
				"--" + boundary + "\r\n" +
				"Content-Type: text/html; charset=utf-8\r\n\r\n" +
				normaliseNewlines(m.HTML) + "\r\n" +
				"--" + boundary + "--\r\n")
	case m.HTML != "":
		headers = append(headers, Header{"Content-Type", "text/html; charset=utf-8"})
		body = []byte(normaliseNewlines(m.HTML))
	default:
		headers = append(headers, Header{"Content-Type", "text/plain; charset=utf-8"})
		body = []byte(normaliseNewlines(m.Text))
	}

	for _, h := range m.Extra {
		if reservedHeaders[strings.ToLower(strings.TrimSpace(h.Name))] {
			return nil, nil, fmt.Errorf("relay: header %q is set by this service and may not be supplied", h.Name)
		}
		headers = append(headers, h)
	}
	return headers, body, nil
}

func (m Message) validate() error {
	switch {
	case strings.TrimSpace(m.From) == "":
		return errors.New("relay: from is required")
	case strings.TrimSpace(m.To) == "":
		return errors.New("relay: to is required")
	case strings.TrimSpace(m.Subject) == "":
		return errors.New("relay: subject is required")
	case m.Text == "" && m.HTML == "":
		return errors.New("relay: a message needs a text or html body")
	}
	for _, addr := range []string{m.From, m.To} {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("relay: %q is not a usable address: %w", addr, err)
		}
	}
	// A bare CR or LF in a header value is header injection: it ends the line
	// and whatever follows becomes a header of its own. Checked here rather
	// than escaped, because a caller that wanted a newline in a Subject wanted
	// something this service should not guess at.
	for _, v := range append([]string{m.Subject, m.From, m.To}, extraValues(m.Extra)...) {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("relay: header values may not contain CR or LF")
		}
	}
	return nil
}

func extraValues(hs []Header) []string {
	out := make([]string, 0, len(hs)*2)
	for _, h := range hs {
		out = append(out, h.Name, h.Value)
	}
	return out
}

// Render writes the wire form: headers, blank line, body. The DKIM-Signature is
// prepended by the caller, because it must be computed from exactly these bytes.
func Render(headers []Header, body []byte) []byte {
	var b strings.Builder
	for _, h := range headers {
		b.WriteString(h.Name)
		b.WriteString(": ")
		b.WriteString(h.Value)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	return []byte(b.String())
}

// encodeSubject RFC 2047-encodes a subject that is not plain ASCII. A raw
// non-ASCII byte in a header is undefined on the wire and arrives as mojibake
// or gets the message rejected.
func encodeSubject(s string) string { return mime.QEncoding.Encode("utf-8", s) }

func normaliseNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("relay: entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
