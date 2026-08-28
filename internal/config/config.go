// Package config is the single source of configuration for verifierd
// (invariant 10: no hardcoded secrets, IPs, or connection strings anywhere else).
//
// Values are resolved in three layers, each overriding the previous: built-in
// defaults, an optional YAML file, then environment variables prefixed with
// VERIFIERD_. The result is validated; the service refuses to boot on a bad or
// missing required value rather than starting in a half-configured state.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrInvalid wraps every validation failure so callers can test for the class
// with errors.Is rather than matching on message text.
var ErrInvalid = errors.New("invalid configuration")

// EnvPrefix is prepended to every environment variable this package reads.
const EnvPrefix = "VERIFIERD_"

// Config is the fully resolved configuration.
type Config struct {
	HTTP     HTTP     `yaml:"http"`
	TLS      TLS      `yaml:"tls"`
	Redis    Redis    `yaml:"redis"`
	DNS      DNS      `yaml:"dns"`
	Pacer    Pacer    `yaml:"pacer"`
	IPHealth IPHealth `yaml:"ip_health"`
	Probe    Probe    `yaml:"probe"`
	Auth     Auth     `yaml:"auth"`
	Log      Log      `yaml:"log"`
}

// TLS configures the listener. ADR-006 settles the boundary as mTLS: the host
// is on a public IP with :25 open and is scanned continuously, so ending the
// handshake before a request reaches the application is worth the setup.
// Leaving CertFile empty serves plain HTTP, which is for local development
// only.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile enables mTLS: only certificates signed by this CA connect.
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether the listener should serve TLS.
func (t TLS) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

// MutualAuth reports whether client certificates are required.
func (t TLS) MutualAuth() bool { return t.ClientCAFile != "" }

// DNS configures how the MX host is resolved.
type DNS struct {
	// Servers are "host:port" resolvers to query. Empty means the process
	// resolver — on the deployed node that is systemd-resolved on 127.0.0.53.
	// Setting it moves this service off a misbehaving stub without touching the
	// rest of the host.
	Servers []string `yaml:"servers"`
	// Timeout bounds one resolution, so a slow resolver cannot spend the
	// probe's whole budget before a socket is opened.
	Timeout time.Duration `yaml:"timeout"`
	// CacheTTL and NegativeTTL bound how long vetted answers and refusals are
	// held in process.
	CacheTTL    time.Duration `yaml:"cache_ttl"`
	NegativeTTL time.Duration `yaml:"negative_ttl"`
	CacheSize   int           `yaml:"cache_size"`
}

// Pacer bounds what the rate limiter keeps in memory. The map is keyed by a
// value that arrives in the request, so it needs a ceiling: without one a bulk
// run over ten thousand domains holds ten thousand entries for the life of the
// process, and every per-MX metric labelled from it becomes a time series that
// never goes away.
type Pacer struct {
	IdleTTL    time.Duration `yaml:"idle_ttl"`
	MaxTracked int           `yaml:"max_tracked"`
}

// IPHealth watches whether the sending IP is still usable.
//
// Checking is **opt-in**: without a resolver that can answer DNSBL queries it
// stays off, because the deployed node's resolver is a stub and a stub answers
// "listed" to every zone. Acting on that would pause the node for a resolver
// misconfiguration.
type IPHealth struct {
	// Resolvers are "host:port" servers able to answer DNSBL queries. Empty
	// disables checking entirely — there is no fallback to the host's resolver.
	Resolvers []string `yaml:"resolvers"`
	// Zones to query. Empty uses the package default (Spamhaus ZEN, SpamCop).
	// UCEPROTECT L3 is deliberately not among them: it lists a whole ASN.
	Zones    []string      `yaml:"zones"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Enabled reports whether DNSBL checking will run.
func (i IPHealth) Enabled() bool { return len(i.Resolvers) > 0 }

// Probe configures the SMTP session.
type Probe struct {
	// Helo must FCrDNS to the sending IP or large providers reject the session
	// with a 5.7.x before RCPT is ever considered.
	Helo string `yaml:"helo"`
	// MailFrom is the envelope sender. It need not FCrDNS, which is what lets
	// verification use a sub-domain isolated from the domain that sends real
	// mail (ARCHITECTURE §"Sender identity").
	MailFrom string `yaml:"mail_from"`
	// SourceIP travels with every answer.
	SourceIP string `yaml:"source_ip"`
	// DialNetwork must be tcp4 (invariant 3).
	DialNetwork string `yaml:"dial_network"`
	// Port is 25 in production. It exists so a staging instance can be aimed
	// at a lab MX (internal/mxsim) without touching anyone else's server.
	Port              string        `yaml:"port"`
	Timeout           time.Duration `yaml:"timeout"`
	MaxRCPTPerSession int           `yaml:"max_rcpt_per_session"`
	// CatchAllProbes is how many known-bad local parts establish whether a
	// domain takes anything. One catches a plain catch-all but not a host that
	// answers by coin flip, where a single probe reports catch-all on one run
	// and clean on the next.
	CatchAllProbes int `yaml:"catch_all_probes"`
	// PolicyStop ends a session after this many consecutive replies that are
	// about our client rather than about a recipient. Zero disables it.
	PolicyStop int `yaml:"policy_stop"`
	// RandomiserTTL is how long a per-server randomiser verdict is remembered.
	RandomiserTTL time.Duration `yaml:"randomiser_ttl"`
	// DeferralRetry is the retry hint returned when a greylisting server gives
	// none of its own. The caller owns the queue (plan 006); this is what lets
	// it schedule instead of backing off blindly into a window that has not
	// opened.
	DeferralRetry       time.Duration `yaml:"deferral_retry"`
	MaxEmailsPerRequest int           `yaml:"max_emails_per_request"`
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
		DNS: DNS{
			Timeout:     5 * time.Second,
			CacheTTL:    5 * time.Minute,
			NegativeTTL: time.Minute,
			CacheSize:   4096,
		},
		Pacer:    Pacer{IdleTTL: 30 * time.Minute, MaxTracked: 512},
		IPHealth: IPHealth{Interval: 15 * time.Minute, Timeout: 5 * time.Second},
		Probe: Probe{
			DialNetwork:         "tcp4",
			Port:                "25",
			Timeout:             20 * time.Second,
			MaxRCPTPerSession:   50,
			CatchAllProbes:      3,
			PolicyStop:          5,
			RandomiserTTL:       24 * time.Hour,
			DeferralRetry:       15 * time.Minute,
			MaxEmailsPerRequest: 500,
		},
		// POST /probe exists from plan 001 on, so the edge is authenticated by
		// default and the service refuses to boot without a key (invariant 11).
		Auth: Auth{Enabled: true},
		Log:  Log{Level: "info", Format: "json"},
	}
}

// Load resolves defaults, then the YAML file at path (skipped when path is
// empty), then environment overrides, and validates the result.
//
// getenv is injected rather than read from the process so tests exercise the
// real precedence without mutating global state; pass os.Getenv in production.
// An empty value counts as unset.
func Load(path string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
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

	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	str := func(key string, dst *string) {
		if v := getenv(EnvPrefix + key); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) error {
		v := getenv(EnvPrefix + key)
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: %s%s: %w", ErrInvalid, EnvPrefix, key, err)
		}
		*dst = d
		return nil
	}
	integer := func(key string, dst *int) error {
		v := getenv(EnvPrefix + key)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%w: %s%s: %w", ErrInvalid, EnvPrefix, key, err)
		}
		*dst = n
		return nil
	}
	boolean := func(key string, dst *bool) error {
		v := getenv(EnvPrefix + key)
		if v == "" {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%w: %s%s: %w", ErrInvalid, EnvPrefix, key, err)
		}
		*dst = b
		return nil
	}

	str("HTTP_ADDR", &cfg.HTTP.Addr)
	str("TLS_CERT_FILE", &cfg.TLS.CertFile)
	str("TLS_KEY_FILE", &cfg.TLS.KeyFile)
	str("TLS_CLIENT_CA_FILE", &cfg.TLS.ClientCAFile)
	str("REDIS_ADDR", &cfg.Redis.Addr)
	if v := getenv(EnvPrefix + "DNS_SERVERS"); v != "" {
		cfg.DNS.Servers = strings.Split(v, ",")
	}
	if v := getenv(EnvPrefix + "IP_HEALTH_RESOLVERS"); v != "" {
		cfg.IPHealth.Resolvers = strings.Split(v, ",")
	}
	if v := getenv(EnvPrefix + "IP_HEALTH_ZONES"); v != "" {
		cfg.IPHealth.Zones = strings.Split(v, ",")
	}
	str("PROBE_HELO", &cfg.Probe.Helo)
	str("PROBE_MAIL_FROM", &cfg.Probe.MailFrom)
	str("PROBE_SOURCE_IP", &cfg.Probe.SourceIP)
	str("PROBE_DIAL_NETWORK", &cfg.Probe.DialNetwork)
	str("PROBE_PORT", &cfg.Probe.Port)
	str("AUTH_API_KEY", &cfg.Auth.APIKey)
	str("LOG_LEVEL", &cfg.Log.Level)
	str("LOG_FORMAT", &cfg.Log.Format)

	for _, f := range []func() error{
		func() error { return dur("HTTP_READ_TIMEOUT", &cfg.HTTP.ReadTimeout) },
		func() error { return dur("HTTP_WRITE_TIMEOUT", &cfg.HTTP.WriteTimeout) },
		func() error { return dur("HTTP_IDLE_TIMEOUT", &cfg.HTTP.IdleTimeout) },
		func() error { return dur("HTTP_SHUTDOWN_TIMEOUT", &cfg.HTTP.ShutdownTimeout) },
		func() error { return dur("REDIS_DIAL_TIMEOUT", &cfg.Redis.DialTimeout) },
		func() error { return dur("DNS_TIMEOUT", &cfg.DNS.Timeout) },
		func() error { return dur("DNS_CACHE_TTL", &cfg.DNS.CacheTTL) },
		func() error { return dur("DNS_NEGATIVE_TTL", &cfg.DNS.NegativeTTL) },
		func() error { return integer("DNS_CACHE_SIZE", &cfg.DNS.CacheSize) },
		func() error { return dur("IP_HEALTH_INTERVAL", &cfg.IPHealth.Interval) },
		func() error { return dur("IP_HEALTH_TIMEOUT", &cfg.IPHealth.Timeout) },
		func() error { return dur("PACER_IDLE_TTL", &cfg.Pacer.IdleTTL) },
		func() error { return integer("PACER_MAX_TRACKED", &cfg.Pacer.MaxTracked) },
		func() error { return dur("PROBE_TIMEOUT", &cfg.Probe.Timeout) },
		func() error { return integer("PROBE_MAX_RCPT_PER_SESSION", &cfg.Probe.MaxRCPTPerSession) },
		func() error { return integer("PROBE_CATCH_ALL_PROBES", &cfg.Probe.CatchAllProbes) },
		func() error { return integer("PROBE_POLICY_STOP", &cfg.Probe.PolicyStop) },
		func() error { return dur("PROBE_RANDOMISER_TTL", &cfg.Probe.RandomiserTTL) },
		func() error { return dur("PROBE_DEFERRAL_RETRY", &cfg.Probe.DeferralRetry) },
		func() error { return integer("PROBE_MAX_EMAILS_PER_REQUEST", &cfg.Probe.MaxEmailsPerRequest) },
		func() error { return boolean("AUTH_ENABLED", &cfg.Auth.Enabled) },
	} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports every reason the configuration cannot be booted on, joined
// into one error. Reporting all of them at once matters operationally: an
// operator fixing a unit file should not have to restart the service seven
// times to discover seven mistakes.
//
// Every returned error wraps ErrInvalid, so callers classify with errors.Is
// instead of matching on message text.
func (c Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...))
	}

	if c.HTTP.Addr == "" {
		add("http.addr must be set")
	}
	if c.Redis.Addr == "" {
		add("redis.addr must be set")
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"http.read_timeout", c.HTTP.ReadTimeout},
		{"http.write_timeout", c.HTTP.WriteTimeout},
		{"http.idle_timeout", c.HTTP.IdleTimeout},
		{"http.shutdown_timeout", c.HTTP.ShutdownTimeout},
		{"redis.dial_timeout", c.Redis.DialTimeout},
	} {
		if d.value <= 0 {
			add("%s must be positive, got %s", d.name, d.value)
		}
	}
	// Refusing to boot beats serving an authenticated surface with no secret.
	if c.Auth.Enabled && c.Auth.APIKey == "" {
		add("auth.enabled is true but auth.api_key is empty")
	}

	// Invariant 3, enforced rather than documented: a bare "tcp" on this
	// dual-stack host prefers IPv6 and leaves from an address with no FCrDNS
	// and no SPF, which providers answer with a 5.7.x. Every result would
	// silently become unusable while looking like a classifier defect.
	if c.Probe.DialNetwork != "tcp4" {
		add("probe.dial_network must be tcp4, got %q (invariant 3)", c.Probe.DialNetwork)
	}
	if c.Probe.Helo == "" {
		add("probe.helo must be set — it must FCrDNS to the sending IP")
	}
	if c.Probe.MailFrom == "" {
		add("probe.mail_from must be set")
	}
	if c.Probe.SourceIP != "" && net.ParseIP(c.Probe.SourceIP) == nil {
		add("probe.source_ip is not an IP address: %q", c.Probe.SourceIP)
	}
	if c.Probe.Timeout <= 0 {
		add("probe.timeout must be positive, got %s", c.Probe.Timeout)
	}
	if c.IPHealth.Interval <= 0 || c.IPHealth.Timeout <= 0 {
		add("ip_health.interval and ip_health.timeout must be positive")
	}
	for _, srv := range c.IPHealth.Resolvers {
		if _, _, err := net.SplitHostPort(srv); err != nil {
			add("ip_health.resolvers entry %q must be host:port: %v", srv, err)
		}
	}
	// Checking needs to know which address it is about.
	if c.IPHealth.Enabled() && c.Probe.SourceIP == "" {
		add("ip_health.resolvers is set but probe.source_ip is empty — there is no address to check")
	}
	if c.Pacer.IdleTTL <= 0 || c.Pacer.MaxTracked <= 0 {
		add("pacer.idle_ttl and pacer.max_tracked must be positive")
	}
	if c.DNS.Timeout <= 0 {
		add("dns.timeout must be positive, got %s", c.DNS.Timeout)
	}
	if c.DNS.CacheTTL <= 0 || c.DNS.NegativeTTL <= 0 || c.DNS.CacheSize <= 0 {
		add("dns.cache_ttl, dns.negative_ttl and dns.cache_size must be positive")
	}
	for _, srv := range c.DNS.Servers {
		if _, _, err := net.SplitHostPort(srv); err != nil {
			add("dns.servers entry %q must be host:port: %v", srv, err)
		}
	}
	if c.Probe.Port == "" {
		add("probe.port must be set")
	}
	// One probe cannot tell a catch-all from a coin flip, which is the whole
	// point of the check.
	if c.Probe.CatchAllProbes < 2 {
		add("probe.catch_all_probes must be at least 2, got %d", c.Probe.CatchAllProbes)
	}
	if c.Probe.RandomiserTTL <= 0 {
		add("probe.randomiser_ttl must be positive")
	}
	// Not negative, and not one: a single 5.7.x can be a per-recipient policy,
	// and stopping a whole batch on it would throw away answers we could have.
	if c.Probe.PolicyStop < 0 || c.Probe.PolicyStop == 1 {
		add("probe.policy_stop must be 0 (disabled) or at least 2, got %d", c.Probe.PolicyStop)
	}
	if c.Probe.DeferralRetry <= 0 {
		add("probe.deferral_retry must be positive")
	}
	if c.Probe.MaxRCPTPerSession <= 0 {
		add("probe.max_rcpt_per_session must be positive")
	}
	if c.Probe.MaxEmailsPerRequest <= 0 {
		add("probe.max_emails_per_request must be positive")
	}

	// A half-configured listener is worse than none: it would serve plain HTTP
	// on a port the caller believes is protected.
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		add("tls.cert_file and tls.key_file must be set together")
	}
	if c.TLS.ClientCAFile != "" && !c.TLS.Enabled() {
		add("tls.client_ca_file requires tls.cert_file and tls.key_file")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format must be json or text, got %q", c.Log.Format)
	}

	return errors.Join(problems...)
}
