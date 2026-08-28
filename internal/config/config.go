// Package config is the single source of configuration for verifierd
// (invariant 10: no hardcoded secrets, IPs, or connection strings anywhere else).
//
// Values are resolved in three layers, each overriding the previous: built-in
// defaults, an optional YAML file, then environment variables prefixed with
// VERIFIERD_. The result is validated; the service refuses to boot on a bad or
// missing required value rather than starting in a half-configured state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is prepended to every environment variable this package reads.
const EnvPrefix = "VERIFIERD_"

// Config is the fully resolved configuration.
type Config struct {
	HTTP  HTTP  `yaml:"http"`
	Redis Redis `yaml:"redis"`
	Auth  Auth  `yaml:"auth"`
	Log   Log   `yaml:"log"`
}

// HTTP configures the JSON API listener.
type HTTP struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Redis is the operational-state store. It holds no business data
// (ARCHITECTURE §"State ownership"); there is no SQL database by design.
type Redis struct {
	// Addr is either "host:port" or "unix:/path/to/socket".
	Addr        string        `yaml:"addr"`
	DialTimeout time.Duration `yaml:"dial_timeout"`
}

// Endpoint splits Addr into a net.Dial network and address.
func (r Redis) Endpoint() (network, address string) {
	if path, ok := strings.CutPrefix(r.Addr, "unix:"); ok {
		return "unix", path
	}
	return "tcp", r.Addr
}

// Auth guards every route except the health probes (invariant 11).
//
// Enabled defaults to false while the only routes are /healthz and /readyz,
// which are unauthenticated by design. Plan 001 adds the first real endpoint
// and makes authentication mandatory.
type Auth struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"api_key"`
}

// Log configures the structured logger.
type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

func defaults() Config {
	return Config{
		HTTP: HTTP{
			Addr:            "127.0.0.1:8080",
			ReadTimeout:     10 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 30 * time.Second,
		},
		Redis: Redis{
			Addr:        "unix:/run/redis/redis-server.sock",
			DialTimeout: 2 * time.Second,
		},
		Auth: Auth{Enabled: false},
		Log:  Log{Level: "info", Format: "json"},
	}
}

// Load resolves defaults, then the YAML file at path (skipped when path is
// empty), then environment overrides, and validates the result.
func Load(path string) (Config, error) {
	cfg := defaults()

	if path != "" {
		// The path is the operator's -config flag, not request input; reading an
		// operator-named file is the entire purpose of the flag.
		raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, not untrusted input
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) error {
		v, ok := os.LookupEnv(EnvPrefix + key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s%s: %w", EnvPrefix, key, err)
		}
		*dst = d
		return nil
	}
	boolean := func(key string, dst *bool) error {
		v, ok := os.LookupEnv(EnvPrefix + key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s%s: %w", EnvPrefix, key, err)
		}
		*dst = b
		return nil
	}

	str("HTTP_ADDR", &cfg.HTTP.Addr)
	str("REDIS_ADDR", &cfg.Redis.Addr)
	str("AUTH_API_KEY", &cfg.Auth.APIKey)
	str("LOG_LEVEL", &cfg.Log.Level)
	str("LOG_FORMAT", &cfg.Log.Format)

	for _, f := range []func() error{
		func() error { return dur("HTTP_READ_TIMEOUT", &cfg.HTTP.ReadTimeout) },
		func() error { return dur("HTTP_WRITE_TIMEOUT", &cfg.HTTP.WriteTimeout) },
		func() error { return dur("HTTP_IDLE_TIMEOUT", &cfg.HTTP.IdleTimeout) },
		func() error { return dur("HTTP_SHUTDOWN_TIMEOUT", &cfg.HTTP.ShutdownTimeout) },
		func() error { return dur("REDIS_DIAL_TIMEOUT", &cfg.Redis.DialTimeout) },
		func() error { return boolean("AUTH_ENABLED", &cfg.Auth.Enabled) },
	} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports the first reason the configuration cannot be booted on.
func (c Config) Validate() error {
	if c.HTTP.Addr == "" {
		return fmt.Errorf("http.addr must be set")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr must be set")
	}
	for name, d := range map[string]time.Duration{
		"http.read_timeout":     c.HTTP.ReadTimeout,
		"http.write_timeout":    c.HTTP.WriteTimeout,
		"http.idle_timeout":     c.HTTP.IdleTimeout,
		"http.shutdown_timeout": c.HTTP.ShutdownTimeout,
		"redis.dial_timeout":    c.Redis.DialTimeout,
	} {
		if d <= 0 {
			return fmt.Errorf("%s must be positive, got %s", name, d)
		}
	}
	// Refusing to boot beats serving an authenticated surface with no secret.
	if c.Auth.Enabled && c.Auth.APIKey == "" {
		return fmt.Errorf("auth.enabled is true but auth.api_key is empty")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log.format must be json or text, got %q", c.Log.Format)
	}
	return nil
}
