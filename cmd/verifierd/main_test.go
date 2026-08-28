package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/config"
)

// freePort returns a port nothing is listening on right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// envFunc builds a getenv for run() so tests never mutate process state, with
// the identity values the service refuses to boot without already filled in.
func envFunc(kv map[string]string) func(string) string {
	base := map[string]string{
		config.EnvPrefix + "PROBE_HELO":      "mail.test",
		config.EnvPrefix + "PROBE_MAIL_FROM": "verify@probe.test",
		config.EnvPrefix + "AUTH_API_KEY":    "test-key",
	}
	maps.Copy(base, kv)
	return func(k string) string { return base[k] }
}

func TestRunServesThenDrainsOnCancel(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"verifierd"}, envFunc(map[string]string{
			config.EnvPrefix + "HTTP_ADDR": addr,
		}), io.Discard)
	}()

	// The server is up once it answers; retry rather than sleeping a fixed time.
	var resp *http.Response
	for range 100 {
		r, err := http.Get("http://" + addr + "/healthz") //nolint:noctx // short-lived probe in a test
		if err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		cancel()
		t.Fatal("server never became reachable")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}

	// Cancelling ctx is what a SIGTERM does in main.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	var stderr bytes.Buffer
	err := run(t.Context(), []string{"verifierd"}, envFunc(map[string]string{
		config.EnvPrefix + "LOG_LEVEL": "verbose",
	}), &stderr)
	if err == nil {
		t.Fatal("run returned nil for an invalid log level")
	}
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("error does not wrap config.ErrInvalid: %v", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(t.Context(), []string{"verifierd", "-nope"}, envFunc(nil), &stderr); err == nil {
		t.Error("run returned nil for an unknown flag")
	}
}
