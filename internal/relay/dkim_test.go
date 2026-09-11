package relay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// The worked example from RFC 6376 §3.4.5. Canonicalization is where DKIM bugs
// live, and they are invisible from this side: the signature is produced
// happily and verifies nowhere.
func TestRelaxedCanonicalizationMatchesTheRFCExample(t *testing.T) {
	headers := []Header{
		{"A", "X"},
		{"B", "Y\t\r\n\tZ  "},
	}
	var got strings.Builder
	for _, h := range headers {
		got.WriteString(canonicalizeHeader(h.Name, h.Value))
	}
	const wantHeaders = "a:X\r\nb:Y Z\r\n"
	if got.String() != wantHeaders {
		t.Errorf("headers:\n got %q\nwant %q", got.String(), wantHeaders)
	}

	body := " C \r\nD \t E\r\n\r\n\r\n"
	const wantBody = " C\r\nD E\r\n"
	if g := string(canonicalizeBody([]byte(body))); g != wantBody {
		t.Errorf("body:\n got %q\nwant %q", g, wantBody)
	}
}

// An empty body canonicalizes to nothing at all — not to a bare CRLF. Getting
// this wrong changes bh= for every message with no body and for none with one,
// which is the kind of bug that survives a whole test suite.
func TestEmptyBodyCanonicalizesToNothing(t *testing.T) {
	for _, in := range []string{"", "\r\n", "\r\n\r\n", "   \r\n\t\r\n"} {
		if got := canonicalizeBody([]byte(in)); len(got) != 0 {
			t.Errorf("canonicalizeBody(%q) = %q, want empty", in, got)
		}
	}
}

func testSigner(t *testing.T) (*Signer, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s, err := NewSigner("datascoutmail.com", "s1", pemKey)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	s.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return s, key
}

// Signing and then verifying the way a *receiver* would: rebuild the signed
// data from the tag, recompute both hashes, check the RSA signature. If this
// passes, the canonicalization the signer used is the one it described in the
// header it wrote — which is the only property a receiver checks.
func TestASignatureVerifiesTheWayAReceiverWouldCheckIt(t *testing.T) {
	signer, key := testSigner(t)
	headers, body, err := Message{
		From: "noreply@datascoutmail.com", To: "someone@example.com",
		Subject: "Reset your password", Text: "Hello.\nHere is a link.\n",
	}.Build(time.Unix(1_700_000_000, 0), "abc123@datascoutmail.com")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	line, err := signer.Sign(headers, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(line, "DKIM-Signature: "), "\r\n")

	// --- what a receiver does ---
	fields := map[string]string{}
	for _, part := range strings.Split(tag, ";") {
		if name, value, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			fields[name] = value
		}
	}
	if fields["c"] != "relaxed/relaxed" || fields["a"] != "rsa-sha256" {
		t.Fatalf("unexpected algorithm tags: %v", fields)
	}

	wantBH := sha256.Sum256(canonicalizeBody(body))
	if fields["bh"] != base64.StdEncoding.EncodeToString(wantBH[:]) {
		t.Fatal("bh= does not match the body it claims to cover")
	}

	present := map[string]Header{}
	for _, h := range headers {
		present[strings.ToLower(h.Name)] = h
	}
	var signed strings.Builder
	for _, name := range strings.Split(fields["h"], ":") {
		h, ok := present[name]
		if !ok {
			t.Fatalf("h= names %q, which is not in the message", name)
		}
		signed.WriteString(canonicalizeHeader(h.Name, h.Value))
	}
	// The signature header itself, with b= emptied and no trailing CRLF.
	signed.WriteString(canonicalizeHeaderNoCRLF("DKIM-Signature",
		tag[:strings.LastIndex(tag, "b=")+2]))

	digest := sha256.Sum256([]byte(signed.String()))
	sig, err := base64.StdEncoding.DecodeString(fields["b"])
	if err != nil {
		t.Fatalf("b= is not base64: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("a receiver would reject this signature: %v", err)
	}
}

// The whole point of signing: a hop that rewrites the message breaks it.
func TestTamperingBreaksTheSignature(t *testing.T) {
	signer, key := testSigner(t)
	headers, body, _ := Message{
		From: "noreply@datascoutmail.com", To: "someone@example.com",
		Subject: "Reset your password", Text: "Hello.\n",
	}.Build(time.Unix(1_700_000_000, 0), "abc123@datascoutmail.com")
	line, err := signer.Sign(headers, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(line, "DKIM-Signature: "), "\r\n")
	bh := ""
	for _, part := range strings.Split(tag, ";") {
		if name, value, ok := strings.Cut(strings.TrimSpace(part), "="); ok && name == "bh" {
			bh = value
		}
	}

	tampered := sha256.Sum256(canonicalizeBody([]byte("Hello.\nAnd a link nobody signed.\n")))
	if base64.StdEncoding.EncodeToString(tampered[:]) == bh {
		t.Fatal("a changed body produced the same body hash")
	}
	_ = key
}

func TestNewSignerRejectsWhatItCannotUse(t *testing.T) {
	for name, pemKey := range map[string]string{
		"not PEM at all": "hello",
		"empty":          "",
		"PEM but not a key": string(pem.EncodeToMemory(
			&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nonsense")})),
	} {
		if _, err := NewSigner("d", "s", []byte(pemKey)); err == nil {
			t.Errorf("%s: NewSigner accepted it", name)
		}
	}
}
