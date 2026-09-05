package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env builds the getenv Load takes, so no test mutates process state. It
// supplies the identity values the service now refuses to boot without, so a
// test only states what it is actually about.
func env(kv map[string]string) func(string) string {
	base := map[string]string{
		EnvPrefix + "PROBE_HELO":      "mail.test",
		EnvPrefix + "PROBE_MAIL_FROM": "verify@probe.test",
		EnvPrefix + "AUTH_API_KEY":    "test-key",
	}
	maps.Copy(base, kv)
	return func(k string) string { return base[k] }
}

// valid is defaults() plus the required identity, i.e. the minimum that boots.
func valid() Config {
	c := defaults()
	c.Probe.Helo = "mail.test"
	c.Probe.MailFrom = "verify@probe.test"
	c.Auth.APIKey = "test-key"
	return c
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("", env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:8080" {
		t.Errorf("HTTP.Addr = %q, want 127.0.0.1:8080", cfg.HTTP.Addr)
	}
	// POST /probe exists from plan 001 on, so the edge is guarded by default.
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled must default to true now that an authenticated route exists")
	}
	if cfg.Probe.DialNetwork != "tcp4" {
		t.Errorf("Probe.DialNetwork = %q, want tcp4 (invariant 3)", cfg.Probe.DialNetwork)
	}
}

// Invariant 3 is enforced by the config, not just documented.
func TestValidateRejectsNonIPv4DialNetwork(t *testing.T) {
	for _, network := range []string{"tcp", "tcp6", "udp", ""} {
		cfg := valid()
		cfg.Probe.DialNetwork = network
		err := cfg.Validate()
		if err == nil {
			t.Errorf("dial_network %q accepted; a bare tcp leaves over IPv6 with no FCrDNS", network)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("dial_network %q: error does not wrap ErrInvalid", network)
		}
	}
}

func TestValidateRejectsHalfConfiguredTLS(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"cert without key": func(c *Config) { c.TLS.CertFile = "/tmp/x.pem" },
		"key without cert": func(c *Config) { c.TLS.KeyFile = "/tmp/x.key" },
		"client CA alone":  func(c *Config) { c.TLS.ClientCAFile = "/tmp/ca.pem" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() = nil; a half-configured listener would serve plain HTTP")
			}
		})
	}
}

func TestPrecedenceDefaultsThenFileThenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verifierd.yaml")
	body := "http:\n  addr: \"0.0.0.0:9999\"\n  read_timeout: 5s\nlog:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, env(map[string]string{EnvPrefix + "HTTP_ADDR": "127.0.0.1:7777"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:7777" {
		t.Errorf("env must override file: got %q", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ReadTimeout != 5*time.Second {
		t.Errorf("file value lost: ReadTimeout = %s", cfg.HTTP.ReadTimeout)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("file value lost: Log.Level = %q", cfg.Log.Level)
	}
	if cfg.Redis.Addr != defaults().Redis.Addr {
		t.Errorf("untouched key must keep its default: got %q", cfg.Redis.Addr)
	}
}

func TestRedisEndpoint(t *testing.T) {
	for _, tc := range []struct{ addr, network, address string }{
		{"unix:/run/redis/redis-server.sock", "unix", "/run/redis/redis-server.sock"},
		{"127.0.0.1:6379", "tcp", "127.0.0.1:6379"},
	} {
		network, address := Redis{Addr: tc.addr}.Endpoint()
		if network != tc.network || address != tc.address {
			t.Errorf("Endpoint(%q) = %q,%q; want %q,%q", tc.addr, network, address, tc.network, tc.address)
		}
	}
}

// The service must refuse to boot rather than run half-configured, and every
// refusal must be classifiable with errors.Is (ENGINEERING-STANDARDS §5).
func TestValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty http addr":     func(c *Config) { c.HTTP.Addr = "" },
		"empty redis addr":    func(c *Config) { c.Redis.Addr = "" },
		"zero timeout":        func(c *Config) { c.HTTP.ReadTimeout = 0 },
		"negative timeout":    func(c *Config) { c.Redis.DialTimeout = -time.Second },
		"auth on without key": func(c *Config) { c.Auth.APIKey = "" },
		"helo missing":        func(c *Config) { c.Probe.Helo = "" },
		"mail_from missing":   func(c *Config) { c.Probe.MailFrom = "" },
		"source_ip not an IP": func(c *Config) { c.Probe.SourceIP = "not-an-ip" },
		"bad log level":       func(c *Config) { c.Log.Level = "verbose" },
		"bad log format":      func(c *Config) { c.Log.Format = "xml" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

// An operator fixing a unit file should see every mistake at once, not one
// restart per mistake.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := valid()
	cfg.HTTP.Addr = ""
	cfg.Redis.Addr = ""
	cfg.Log.Level = "verbose"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want errors")
	}
	for _, want := range []string{"http.addr", "redis.addr", "log.level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error is missing %q: %v", want, err)
		}
	}
}

func TestValidateAcceptsAMinimalRealConfig(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadInvalidEnvIsAnError(t *testing.T) {
	_, err := Load("", env(map[string]string{EnvPrefix + "HTTP_READ_TIMEOUT": "not-a-duration"}))
	if err == nil {
		t.Fatal("Load() = nil error for an unparseable duration")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error does not wrap ErrInvalid: %v", err)
	}
}

func TestLoadNilGetenvFallsBackToProcess(t *testing.T) {
	t.Setenv(EnvPrefix+"HTTP_ADDR", "127.0.0.1:6060")
	t.Setenv(EnvPrefix+"PROBE_HELO", "mail.test")
	t.Setenv(EnvPrefix+"PROBE_MAIL_FROM", "verify@probe.test")
	t.Setenv(EnvPrefix+"AUTH_API_KEY", "test-key")
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:6060" {
		t.Errorf("HTTP.Addr = %q, want the process environment value", cfg.HTTP.Addr)
	}
}
