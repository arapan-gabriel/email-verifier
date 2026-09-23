package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/config"
)

// testCA is a certificate authority generated in-test — no fixtures on disk
// (plan 027, Design 6).
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf for either a server (127.0.0.1) or a client.
func (ca *testCA) issue(t *testing.T, server bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientAuthTLSConfigShapes(t *testing.T) {
	if c, err := clientAuthTLS(config.TLS{}); c != nil || err != nil {
		t.Errorf("no TLS = %v, %v; want nil, nil", c, err)
	}
	c, err := clientAuthTLS(config.TLS{CertFile: "c", KeyFile: "k"})
	if err != nil || c.MinVersion != tls.VersionTLS13 || c.ClientAuth != tls.NoClientCert {
		t.Errorf("TLS without a CA = %+v, %v; want TLS 1.3 and no client auth", c, err)
	}
	dir := t.TempDir()
	if _, err := clientAuthTLS(config.TLS{CertFile: "c", KeyFile: "k", ClientCAFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing CA file must be an error")
	}
	junk := writeFile(t, dir, "junk.pem", []byte("not a certificate"))
	if _, err := clientAuthTLS(config.TLS{CertFile: "c", KeyFile: "k", ClientCAFile: junk}); err == nil {
		t.Error("a CA file with no certificate must be an error")
	}
	ca := writeFile(t, dir, "ca.pem", newCA(t, "ca").pem)
	c, err = clientAuthTLS(config.TLS{CertFile: "c", KeyFile: "k", ClientCAFile: ca})
	if err != nil || c.ClientAuth != tls.RequireAndVerifyClientCert || c.ClientCAs == nil {
		t.Errorf("mTLS = %+v, %v; want RequireAndVerifyClientCert with the pool", c, err)
	}
}

// The mTLS edge through a real handshake: refused with no client certificate,
// refused with one from a different CA, accepted with one from the configured
// CA (invariant 11, ADR-006).
func TestClientAuthTLSHandshake(t *testing.T) {
	ca, other := newCA(t, "trusted"), newCA(t, "stranger")
	cfg, err := clientAuthTLS(config.TLS{CertFile: "c", KeyFile: "k", ClientCAFile: writeFile(t, t.TempDir(), "ca.pem", ca.pem)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "in")
	}))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	get := func(certs ...tls.Certificate) error {
		client := srv.Client()
		tr := client.Transport.(*http.Transport).Clone()
		tr.TLSClientConfig.Certificates = certs
		client.Transport = tr
		defer tr.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
		return err
	}

	if err := get(); err == nil {
		t.Error("a handshake without a client certificate was accepted")
	}
	stranger, _, _ := other.issue(t, false)
	if err := get(stranger); err == nil {
		t.Error("a client certificate from a different CA was accepted")
	}
	good, _, _ := ca.issue(t, false)
	if err := get(good); err != nil {
		t.Errorf("a client certificate from the configured CA was refused: %v", err)
	}
}

// run() itself serves mTLS end to end when tls.* is configured.
func TestRunServesMutualTLS(t *testing.T) {
	ca := newCA(t, "edge")
	dir := t.TempDir()
	_, certPEM, keyPEM := ca.issue(t, true)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"verifierd"}, envFunc(map[string]string{
			config.EnvPrefix + "HTTP_ADDR":          addr,
			config.EnvPrefix + "TLS_CERT_FILE":      writeFile(t, dir, "cert.pem", certPEM),
			config.EnvPrefix + "TLS_KEY_FILE":       writeFile(t, dir, "key.pem", keyPEM),
			config.EnvPrefix + "TLS_CLIENT_CA_FILE": writeFile(t, dir, "ca.pem", ca.pem),
		}), io.Discard)
	}()

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	client, _, _ := ca.issue(t, false)
	get := func(certs ...tls.Certificate) error {
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: certs, MinVersion: tls.VersionTLS13}}
		defer tr.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/healthz", nil)
		resp, err := (&http.Client{Transport: tr, Timeout: 2 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
		return err
	}

	var err error
	for range 200 {
		if err = get(client); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("mTLS healthz never answered: %v", err)
	}
	if err := get(); err == nil {
		t.Error("run's listener accepted a client with no certificate")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run = %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

// A CA file that cannot be used stops run before it listens.
func TestRunRefusesAnUnusableClientCA(t *testing.T) {
	dir := t.TempDir()
	err := run(t.Context(), []string{"verifierd"}, envFunc(map[string]string{
		config.EnvPrefix + "HTTP_ADDR":          fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		config.EnvPrefix + "TLS_CERT_FILE":      writeFile(t, dir, "c", []byte("x")),
		config.EnvPrefix + "TLS_KEY_FILE":       writeFile(t, dir, "k", []byte("x")),
		config.EnvPrefix + "TLS_CLIENT_CA_FILE": writeFile(t, dir, "ca", []byte("x")),
	}), io.Discard)
	if err == nil {
		t.Fatal("run accepted a client CA with no certificate in it")
	}
}
