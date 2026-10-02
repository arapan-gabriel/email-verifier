package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/config"
	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/relay"
)

// syncBuffer is a log sink safe to read while run() is still writing to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func dkimKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// closedAddr is a TCP address nothing listens on, so every dependency behind it
// fails fast and deterministically.
func closedAddr(t *testing.T) string { return fmt.Sprintf("127.0.0.1:%d", freePort(t)) }

// Every optional subsystem switched on at once: relay (with its drain), a
// loadable suppression list, and blocklist checking whose resolver cannot
// answer. The node still boots, says what it disabled, and drains cleanly.
func TestRunWithEverySubsystemConfigured(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeFile(t, dir, "dkim.pem", dkimKeyPEM(t))
	cfgFile := writeFile(t, dir, "verifierd.yaml", []byte(fmt.Sprintf(`
relay:
  enabled: true
  domain: mail.test
  dkim_key_file: %s
  dkim_selector: s1
  return_path_domain: bounce.mail.test
log:
  format: text
  level: debug
`, keyFile)))
	addr := closedAddr(t)
	logs := &syncBuffer{}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"verifierd", "-config", cfgFile}, envFunc(map[string]string{
			config.EnvPrefix + "HTTP_ADDR":           addr,
			config.EnvPrefix + "REDIS_ADDR":          closedAddr(t),
			config.EnvPrefix + "REDIS_DIAL_TIMEOUT":  "200ms",
			config.EnvPrefix + "SUPPRESS_SALT":       "salt",
			config.EnvPrefix + "IP_HEALTH_RESOLVERS": closedAddr(t),
			config.EnvPrefix + "IP_HEALTH_TIMEOUT":   "200ms",
			config.EnvPrefix + "PROBE_SOURCE_IP":     "192.0.2.1",
			config.EnvPrefix + "AUTH_ENABLED":        "false",
		}), logs)
	}()

	var resp *http.Response
	for range 300 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/readyz", nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		cancel()
		t.Fatalf("never listened; logs:\n%s", logs)
	}
	_ = resp.Body.Close()
	// Redis is unreachable, so the node must say it is not ready (invariant 5).
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz with no Redis = %d, want 503", resp.StatusCode)
	}
	// POST /send is registered because the relay is on.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/send", strings.NewReader(`{`))
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /send = %v %v, want 400 from a registered route", r, err)
	} else {
		_ = r.Body.Close()
	}
	// The bands view is wired and answers an empty list, not null.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/admin/bands", nil)
	if r, err := http.DefaultClient.Do(req); err == nil {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = r.Body.Close()
		if string(body["bands"]) != "[]" {
			t.Errorf("GET /admin/bands = %s", body["bands"])
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
	out := logs.String()
	for _, want := range []string{
		"authentication disabled", "serving plain HTTP", "blocklist checking disabled",
		"suppression list loadable but not enforced", "outbound relay enabled", "stopped cleanly",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

func TestRunRelayStartupFailures(t *testing.T) {
	dir := t.TempDir()
	for name, key := range map[string]string{
		"missing key": dir + "/nope.pem",
		"not a key":   writeFile(t, dir, "junk.pem", []byte("junk")),
	} {
		t.Run(name, func(t *testing.T) {
			cfgFile := writeFile(t, t.TempDir(), "v.yaml", []byte(fmt.Sprintf(
				"relay:\n  enabled: true\n  domain: m.test\n  dkim_key_file: %s\n  dkim_selector: s\n  return_path_domain: b.test\n", key)))
			err := run(t.Context(), []string{"verifierd", "-config", cfgFile}, envFunc(map[string]string{
				config.EnvPrefix + "HTTP_ADDR": closedAddr(t),
			}), io.Discard)
			if err == nil {
				t.Fatal("run booted with an unusable DKIM key")
			}
		})
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	err = run(t.Context(), []string{"verifierd"}, envFunc(map[string]string{
		config.EnvPrefix + "HTTP_ADDR": l.Addr().String(),
	}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run on a taken port = %v, want a listen error", err)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	for level, enabled := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo,
	} {
		l := newLogger(config.Log{Level: level, Format: "json"}, io.Discard)
		if !l.Enabled(context.Background(), enabled) || (enabled > slog.LevelDebug && l.Enabled(context.Background(), enabled-4)) {
			t.Errorf("level %q: wrong threshold", level)
		}
	}
}

func TestDNSBLLookup(t *testing.T) {
	if dnsblLookup(config.IPHealth{}) != nil {
		t.Error("no resolver must mean no lookup — never the host's stub")
	}
	lookup := dnsblLookup(config.IPHealth{Resolvers: []string{closedAddr(t)}, Timeout: 200 * time.Millisecond})
	if lookup == nil {
		t.Fatal("a configured resolver must give a lookup")
	}
	if _, err := lookup(t.Context(), "2.0.0.127.zen.example."); err == nil {
		t.Error("a lookup through a dead resolver must fail")
	}
}

// ---- drain ----

// fakeRelayStore is the relay queue's two-method store, holding a list of ids
// and their blobs. err, when set, fails every call.
type fakeRelayStore struct {
	mu    sync.Mutex
	ready []string
	blobs map[string]string
	err   error
}

func (s *fakeRelayStore) Do(_ context.Context, args ...string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	switch args[0] {
	case "RPOP":
		if len(s.ready) == 0 {
			return nil, nil
		}
		id := s.ready[0]
		s.ready = s.ready[1:]
		return id, nil
	case "DEL":
		delete(s.blobs, strings.TrimPrefix(args[1], "relay:msg:"))
	case "LLEN", "ZCARD":
		return int64(0), nil
	}
	return nil, nil
}

func (s *fakeRelayStore) Get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[strings.TrimPrefix(key, "relay:msg:")]
	return b, ok, nil
}

type burnedHealth struct{ burned bool }

func (b burnedHealth) Burned() (bool, string)        { return b.burned, "listed" }
func (b burnedHealth) SendingPaused() (bool, string) { return false, "" }

type suppressAll struct{}

func (suppressAll) Suppressed(context.Context, string) (bool, string, error) {
	return true, "erased", nil
}
func (suppressAll) Enforcing() bool            { return true }
func (suppressAll) Stale(context.Context) bool { return false }

// The drain delivers back to back while there is work, waits its interval when
// there is none, stays quiet about ErrNotSending, logs real errors, and stops
// on cancellation.
func TestDrain(t *testing.T) {
	t.Run("works through the queue, then waits, then stops", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := &fakeRelayStore{ready: []string{"a", "b"}, blobs: map[string]string{
				"a": `{"id":"a","rcpt_to":"x@y.test"}`, "b": `{"id":"b","rcpt_to":"z@y.test"}`,
			}}
			r := relay.New(relay.Options{Queue: relay.NewQueue(store, 3), Suppress: suppressAll{}})
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { drain(ctx, r, time.Minute, slog.New(slog.NewTextHandler(&logs, nil))); close(done) }()
			synctest.Wait()
			if len(store.blobs) != 0 {
				t.Errorf("both suppressed messages should be dropped back to back, %d left", len(store.blobs))
			}
			cancel()
			<-done
			if logs.Len() != 0 {
				t.Errorf("unexpected log: %s", logs.String())
			}
		})
	})
	t.Run("not sending is a state, not a log line", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := relay.New(relay.Options{Queue: relay.NewQueue(&fakeRelayStore{}, 3), Health: burnedHealth{true}})
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { drain(ctx, r, 0, slog.New(slog.NewTextHandler(&logs, nil))); close(done) }()
			time.Sleep(35 * time.Second) // three default intervals on the fake clock
			cancel()
			<-done
			if logs.Len() != 0 {
				t.Errorf("ErrNotSending was logged: %s", logs.String())
			}
		})
	})
	t.Run("a broken store is logged each interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := relay.New(relay.Options{Queue: relay.NewQueue(&fakeRelayStore{err: errors.New("down")}, 3)})
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { drain(ctx, r, time.Second, slog.New(slog.NewTextHandler(&logs, nil))); close(done) }()
			time.Sleep(2500 * time.Millisecond)
			cancel()
			<-done
			if n := strings.Count(logs.String(), "relay drain"); n != 3 {
				t.Errorf("logged %d times in 2.5 intervals, want 3 (t=0, 1s, 2s)", n)
			}
		})
	})
}

// ---- adapters ----

type mapStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *mapStore) Get(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *mapStore) Set(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

type allowAll struct{}

func (allowAll) Take(context.Context, string, float64, float64) (limiter.Decision, error) {
	return limiter.Decision{Allowed: true}, nil
}

func TestBandsViewAdaptsThePacer(t *testing.T) {
	p := pacer.New(&mapStore{m: map[string]string{}}, allowAll{}, pacer.Options{})
	v := bandsView{p: p}
	if rows := v.Snapshot(); len(rows) != 0 {
		t.Fatalf("fresh pacer rows = %v", rows)
	}
	if err := p.Acquire(t.Context(), "mx.test", "example.test"); err != nil {
		t.Fatal(err)
	}
	rows := v.Snapshot()
	if len(rows) != 1 || rows[0].MXHost != "mx.test" || rows[0].MaxRate <= 0 {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := v.Promote(t.Context(), "mx.test"); err == nil {
		t.Error("promoting with no proposal must be refused")
	}
}
