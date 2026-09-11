// Package relay assembles, signs and sends outbound mail from the isolated IP
// (plan 014).
//
// It shares the pacer and the limiter with verification — the receiving server
// sees one address and does not care which of our two activities it is — and
// nothing else. Verification never sends DATA (invariant 8); this package only
// sends DATA. Keeping them apart in code is what makes that statement checkable
// rather than a promise.
package relay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Signer holds the DKIM key and the identity it signs for.
type Signer struct {
	Domain   string
	Selector string
	key      *rsa.PrivateKey
	// Now is injected so a test can assert the t= tag without a clock race.
	Now func() time.Time
}

// NewSigner parses a PEM private key. PKCS#8 first, because that is what
// `openssl genrsa` writes today ("BEGIN PRIVATE KEY"); PKCS#1 is accepted so an
// older key does not have to be regenerated to be usable.
func NewSigner(domain, selector string, pemKey []byte) (*Signer, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("dkim: no PEM block in the key")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("dkim: key is %T, not RSA", parsed)
		}
		key = rsaKey
	} else if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else {
		return nil, fmt.Errorf("dkim: unusable private key: %w", err)
	}
	if domain == "" || selector == "" {
		return nil, errors.New("dkim: domain and selector are required")
	}
	return &Signer{Domain: domain, Selector: selector, key: key, Now: time.Now}, nil
}

// signedHeaders is the h= list, in signing order.
//
// From is the one a receiver aligns against DMARC, so it is not optional. Date
// and Message-ID are signed because a replay that changes either is a different
// message. Subject and To are signed because a relay that could rewrite them
// while keeping the signature valid would make the signature worthless.
var signedHeaders = []string{"from", "to", "subject", "date", "message-id", "mime-version", "content-type"}

// Sign returns the DKIM-Signature header line, with CRLF, ready to prepend.
//
// relaxed/relaxed canonicalization: strict/simple breaks the moment any hop
// re-folds a header or touches trailing whitespace, which is most of them.
func (s *Signer) Sign(headers []Header, body []byte) (string, error) {
	bodyHash := sha256.Sum256(canonicalizeBody(body))

	present := map[string]Header{}
	for _, h := range headers {
		present[strings.ToLower(h.Name)] = h
	}
	var hList []string
	var canon strings.Builder
	for _, name := range signedHeaders {
		h, ok := present[name]
		if !ok {
			// Absent headers are left out of h= entirely rather than signed as
			// empty: signing a header that is not there invites a receiver to
			// resolve the mismatch by rejecting the message.
			continue
		}
		hList = append(hList, name)
		canon.WriteString(canonicalizeHeader(h.Name, h.Value))
	}

	tag := fmt.Sprintf(
		"v=1; a=rsa-sha256; c=relaxed/relaxed; d=%s; s=%s; t=%d; h=%s; bh=%s; b=",
		s.Domain, s.Selector, s.Now().Unix(), strings.Join(hList, ":"),
		base64.StdEncoding.EncodeToString(bodyHash[:]),
	)

	// The signature header signs itself, with an empty b= and **no trailing
	// CRLF** — RFC 6376 §3.7. Getting that CRLF wrong produces a signature that
	// verifies nowhere and is invisible from this side.
	canon.WriteString(canonicalizeHeaderNoCRLF("DKIM-Signature", tag))

	digest := sha256.Sum256([]byte(canon.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("dkim: signing: %w", err)
	}
	return "DKIM-Signature: " + tag + base64.StdEncoding.EncodeToString(sig) + "\r\n", nil
}

// canonicalizeHeader implements relaxed header canonicalization (RFC 6376
// §3.4.2): lowercase the name, unfold, collapse runs of whitespace to one
// space, strip whitespace around the colon and at the end.
func canonicalizeHeader(name, value string) string {
	return canonicalizeHeaderNoCRLF(name, value) + "\r\n"
}

func canonicalizeHeaderNoCRLF(name, value string) string {
	// Unfolding first: a folded header is one logical value, and collapsing
	// before unfolding would leave the fold's CRLF inside it.
	value = strings.ReplaceAll(value, "\r\n", "")
	value = strings.ReplaceAll(value, "\n", "")

	var b strings.Builder
	space := false
	for _, r := range value {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return strings.ToLower(strings.TrimSpace(name)) + ":" + b.String()
}

// canonicalizeBody implements relaxed body canonicalization (RFC 6376 §3.4.4):
// strip trailing whitespace on every line, collapse internal whitespace runs,
// and remove trailing empty lines — then ensure exactly one CRLF at the end.
//
// An empty body canonicalizes to nothing at all, not to a bare CRLF.
func canonicalizeBody(body []byte) []byte {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		var b strings.Builder
		space := false
		for _, r := range line {
			if r == ' ' || r == '\t' {
				space = true
				continue
			}
			// A run of whitespace becomes **one space, including at the start
			// of a line**. Deleting a leading run is the header rule (§3.4.2
			// strips whitespace around the colon) and applying it here changes
			// bh= for every indented line — quoted replies, signatures, most
			// HTML. Caught by the RFC's own worked example.
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
		// Trailing whitespace needs no trimming: a run is only written when a
		// non-whitespace character follows it, so one at the end is dropped.
		lines[i] = b.String()
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}
