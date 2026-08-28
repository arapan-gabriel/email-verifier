package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:8080" {
		t.Errorf("HTTP.Addr = %q, want 127.0.0.1:8080", cfg.HTTP.Addr)
	}
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled defaults to true; it must default to false until plan 001")
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verifierd.yaml")
	body := "http:\n  addr: \"0.0.0.0:9999\"\n  read_timeout: 5s\nlog:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"HTTP_ADDR", "127.0.0.1:7777")

	cfg, err := Load(path)
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

// The service must refuse to boot rather than run half-configured.
func TestValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty http addr":     func(c *Config) { c.HTTP.Addr = "" },
		"empty redis addr":    func(c *Config) { c.Redis.Addr = "" },
		"zero timeout":        func(c *Config) { c.HTTP.ReadTimeout = 0 },
		"negative timeout":    func(c *Config) { c.Redis.DialTimeout = -time.Second },
		"auth on without key": func(c *Config) { c.Auth.Enabled = true },
		"bad log level":       func(c *Config) { c.Log.Level = "verbose" },
		"bad log format":      func(c *Config) { c.Log.Format = "xml" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaults()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}

func TestValidateAcceptsAuthWithKey(t *testing.T) {
	cfg := defaults()
	cfg.Auth.Enabled = true
	cfg.Auth.APIKey = "s3cret"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadInvalidEnvIsAnError(t *testing.T) {
	t.Setenv(EnvPrefix+"HTTP_READ_TIMEOUT", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Error("Load() = nil error for an unparseable duration")
	}
}
