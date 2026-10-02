package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One row per Validate rule (plan 027, Design 7). Each is checked with
// errors.Is against ErrInvalid and by the key it names, never by the rest of
// the message (ENGINEERING-STANDARDS §4).
func TestValidateEveryRule(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		key    string
	}{
		{"http addr", func(c *Config) { c.HTTP.Addr = "" }, "http.addr"},
		{"redis addr", func(c *Config) { c.Redis.Addr = "" }, "redis.addr"},
		{"read timeout", func(c *Config) { c.HTTP.ReadTimeout = 0 }, "http.read_timeout"},
		{"write timeout", func(c *Config) { c.HTTP.WriteTimeout = -1 }, "http.write_timeout"},
		{"idle timeout", func(c *Config) { c.HTTP.IdleTimeout = 0 }, "http.idle_timeout"},
		{"shutdown timeout", func(c *Config) { c.HTTP.ShutdownTimeout = 0 }, "http.shutdown_timeout"},
		{"redis dial timeout", func(c *Config) { c.Redis.DialTimeout = 0 }, "redis.dial_timeout"},
		{"auth with no key", func(c *Config) { c.Auth.APIKey = "" }, "auth.api_key"},
		// Invariant 3's regression row: anything but tcp4 refuses to boot.
		{"dial tcp", func(c *Config) { c.Probe.DialNetwork = "tcp" }, "probe.dial_network"},
		{"dial tcp6", func(c *Config) { c.Probe.DialNetwork = "tcp6" }, "probe.dial_network"},
		{"helo", func(c *Config) { c.Probe.Helo = "" }, "probe.helo"},
		{"mail from", func(c *Config) { c.Probe.MailFrom = "" }, "probe.mail_from"},
		{"source ip", func(c *Config) { c.Probe.SourceIP = "not-an-ip" }, "probe.source_ip"},
		{"probe timeout", func(c *Config) { c.Probe.Timeout = 0 }, "probe.timeout"},
		{"suppress without salt", func(c *Config) { c.Suppress.Enabled = true }, "suppress.salt"},
		{"suppress stale", func(c *Config) { c.Suppress.Stale = 0 }, "suppress.stale"},
		{"suppress max hashes", func(c *Config) { c.Suppress.MaxHashesPerImport = 0 }, "suppress.max_hashes_per_import"},
		{"ip health interval", func(c *Config) { c.IPHealth.Interval = 0 }, "ip_health.interval"},
		{"ip health timeout", func(c *Config) { c.IPHealth.Timeout = 0 }, "ip_health.timeout"},
		{"ip health resolver shape", func(c *Config) {
			c.IPHealth.Resolvers = []string{"1.1.1.1"}
			c.Probe.SourceIP = "192.0.2.1"
		}, "ip_health.resolvers entry"},
		{"ip health without source ip", func(c *Config) { c.IPHealth.Resolvers = []string{"127.0.0.1:53"} }, "probe.source_ip is empty"},
		{"pacer idle ttl", func(c *Config) { c.Pacer.IdleTTL = 0 }, "pacer.idle_ttl"},
		{"pacer max tracked", func(c *Config) { c.Pacer.MaxTracked = 0 }, "pacer.max_tracked"},
		{"pacer promote after", func(c *Config) { c.Pacer.PromoteAfter = -1 }, "pacer.promote_after"},
		{"pacer promote step", func(c *Config) { c.Pacer.PromoteStep = 1 }, "pacer.promote_step"},
		{"pacer promote ceiling", func(c *Config) { c.Pacer.PromoteCeiling = 0 }, "pacer.promote_ceiling"},
		{"dns timeout", func(c *Config) { c.DNS.Timeout = 0 }, "dns.timeout"},
		{"dns cache ttl", func(c *Config) { c.DNS.CacheTTL = 0 }, "dns.cache_ttl"},
		{"dns negative ttl", func(c *Config) { c.DNS.NegativeTTL = 0 }, "dns.negative_ttl"},
		{"dns cache size", func(c *Config) { c.DNS.CacheSize = 0 }, "dns.cache_size"},
		{"dns server shape", func(c *Config) { c.DNS.Servers = []string{"1.1.1.1"} }, "dns.servers entry"},
		{"probe port", func(c *Config) { c.Probe.Port = "" }, "probe.port"},
		{"catch-all probes", func(c *Config) { c.Probe.CatchAllProbes = 1 }, "probe.catch_all_probes"},
		{"catch-all audit negative", func(c *Config) { c.Probe.CatchAllAuditRate = -0.1 }, "probe.catch_all_audit_rate"},
		// Plan 028: a lease that dies under a live session hands its room out twice.
		{"session lease not longer than the session", func(c *Config) { c.Pacer.SessionLease = c.Probe.Timeout }, "pacer.session_lease"},
		{"lease wait zero", func(c *Config) { c.Pacer.LeaseWait = 0 }, "pacer.lease_wait"},
		{"stand-down on one host", func(c *Config) { c.StandDown.Hosts = 1 }, "standdown.hosts"},
		{"stand-down window zero", func(c *Config) { c.StandDown.Window = 0 }, "standdown.window"},
		{"stand-down pause zero", func(c *Config) { c.StandDown.Pause = 0 }, "standdown.pause"},
		// ...and a wait plus a session must still answer inside Data Scout's 90 s.
		{"lease wait plus session past the caller", func(c *Config) { c.Pacer.LeaseWait = 75 * time.Second }, "pacer.lease_wait"},
		{"catch-all audit above one", func(c *Config) { c.Probe.CatchAllAuditRate = 1.5 }, "probe.catch_all_audit_rate"},
		// Plan 031: two modes only, and a handshake that fits inside the session.
		{"starttls unknown mode", func(c *Config) { c.Probe.StartTLS = "required" }, "probe.starttls"},
		{"tls handshake zero", func(c *Config) { c.Probe.TLSHandshakeTimeout = 0 }, "probe.tls_handshake_timeout"},
		{"tls handshake not inside the session", func(c *Config) { c.Probe.TLSHandshakeTimeout = c.Probe.Timeout }, "probe.tls_handshake_timeout"},
		{"randomiser ttl", func(c *Config) { c.Probe.RandomiserTTL = 0 }, "probe.randomiser_ttl"},
		{"policy stop one", func(c *Config) { c.Probe.PolicyStop = 1 }, "probe.policy_stop must"},
		{"policy stop negative", func(c *Config) { c.Probe.PolicyStop = -1 }, "probe.policy_stop must"},
		{"policy stop max negative", func(c *Config) { c.Probe.PolicyStopMax = -1 }, "probe.policy_stop_max must not be negative"},
		{"policy stop max below default", func(c *Config) { c.Probe.PolicyStop, c.Probe.PolicyStopMax = 5, 4 }, "must not be below probe.policy_stop"},
		{"deferral retry", func(c *Config) { c.Probe.DeferralRetry = 0 }, "probe.deferral_retry"},
		{"max rcpt", func(c *Config) { c.Probe.MaxRCPTPerSession = 0 }, "probe.max_rcpt_per_session"},
		{"max emails", func(c *Config) { c.Probe.MaxEmailsPerRequest = 0 }, "probe.max_emails_per_request"},
		{"cert without key", func(c *Config) { c.TLS.CertFile = "c.pem" }, "tls.cert_file and tls.key_file"},
		{"key without cert", func(c *Config) { c.TLS.KeyFile = "k.pem" }, "tls.cert_file and tls.key_file"},
		{"client ca without a listener cert", func(c *Config) { c.TLS.ClientCAFile = "ca.pem" }, "tls.client_ca_file"},
		{"log level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"relay domain", func(c *Config) { relayOn(c); c.Relay.Domain = "" }, "relay.domain"},
		{"relay dkim key", func(c *Config) { relayOn(c); c.Relay.DKIMKeyFile = "" }, "relay.dkim_key_file"},
		{"relay dkim selector", func(c *Config) { relayOn(c); c.Relay.DKIMSelector = "" }, "relay.dkim_selector"},
		{"relay return path", func(c *Config) { relayOn(c); c.Relay.ReturnPathDomain = "" }, "relay.return_path_domain"},
		{"relay attempts", func(c *Config) { relayOn(c); c.Relay.MaxAttempts = 0 }, "relay.max_attempts"},
		{"relay retry base", func(c *Config) { relayOn(c); c.Relay.RetryBase = 0 }, "relay.retry_base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %q: %v", tc.key, err)
			}
		})
	}
}

func relayOn(c *Config) {
	c.Relay.Enabled = true
	c.Relay.Domain = "mail.test"
	c.Relay.DKIMKeyFile = "dkim.pem"
	c.Relay.DKIMSelector = "s1"
	c.Relay.ReturnPathDomain = "bounce.mail.test"
}

// Rows that must be accepted: the boundaries next to the refusals above.
func TestValidateAcceptsBoundaries(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relay fully configured":          relayOn,
		"policy stop disabled":            func(c *Config) { c.Probe.PolicyStop = 0 },
		"policy stop max zero":            func(c *Config) { c.Probe.PolicyStopMax = 0 },
		"policy stop max equal":           func(c *Config) { c.Probe.PolicyStop, c.Probe.PolicyStopMax = 5, 5 },
		"promote disabled":                func(c *Config) { c.Pacer.PromoteAfter = 0 },
		"mTLS":                            func(c *Config) { c.TLS = TLS{CertFile: "c", KeyFile: "k", ClientCAFile: "ca"} },
		"ip health named with its ip":     func(c *Config) { c.IPHealth.Resolvers = []string{"127.0.0.1:53"}; c.Probe.SourceIP = "192.0.2.1" },
		"suppress with a salt":            func(c *Config) { c.Suppress.Enabled, c.Suppress.Salt = true, "s" },
		"dns servers host:port":           func(c *Config) { c.DNS.Servers = []string{"1.1.1.1:53", "[2606:4700::1111]:53"} },
		"text logs at debug":              func(c *Config) { c.Log.Format, c.Log.Level = "text", "debug" },
		"auth off, no key needed":         func(c *Config) { c.Auth = Auth{} },
		"relay settings ignored when off": func(c *Config) { c.Relay.MaxAttempts = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate = %v, want nil", err)
			}
		})
	}
}

// Auth off with no mTLS boots today, and run() only warns. Invariant 11 says
// every request is authenticated; making this a refusal changes the boot
// contract of a live service, so plan 027 pins the current behaviour and
// tech-debt.md carries the recommendation. When that lands, this row flips.
func TestValidateAuthOffWithoutMTLSIsAcceptedToday(t *testing.T) {
	cfg := valid()
	cfg.Auth.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v; if auth-off is now refused, flip this row and close the tech-debt item", err)
	}
}

// Every environment override is read, and a bad value for any typed one is
// ErrInvalid naming the variable.
func TestEnvOverridesEveryKey(t *testing.T) {
	cfg, err := Load("", env(map[string]string{
		EnvPrefix + "HTTP_ADDR":                    "0.0.0.0:9",
		EnvPrefix + "TLS_CERT_FILE":                "c.pem",
		EnvPrefix + "TLS_KEY_FILE":                 "k.pem",
		EnvPrefix + "TLS_CLIENT_CA_FILE":           "ca.pem",
		EnvPrefix + "REDIS_ADDR":                   "127.0.0.1:6380",
		EnvPrefix + "DNS_SERVERS":                  "1.1.1.1:53,9.9.9.9:53",
		EnvPrefix + "IP_HEALTH_RESOLVERS":          "127.0.0.1:53",
		EnvPrefix + "IP_HEALTH_ZONES":              "zen.example,bl.example",
		EnvPrefix + "SUPPRESS_SALT":                "salt",
		EnvPrefix + "PROBE_SOURCE_IP":              "192.0.2.1",
		EnvPrefix + "PROBE_DIAL_NETWORK":           "tcp4",
		EnvPrefix + "PROBE_PORT":                   "2525",
		EnvPrefix + "LOG_LEVEL":                    "warn",
		EnvPrefix + "LOG_FORMAT":                   "text",
		EnvPrefix + "HTTP_READ_TIMEOUT":            "1s",
		EnvPrefix + "HTTP_WRITE_TIMEOUT":           "2s",
		EnvPrefix + "HTTP_IDLE_TIMEOUT":            "3s",
		EnvPrefix + "HTTP_SHUTDOWN_TIMEOUT":        "4s",
		EnvPrefix + "REDIS_DIAL_TIMEOUT":           "5s",
		EnvPrefix + "DNS_TIMEOUT":                  "6s",
		EnvPrefix + "DNS_CACHE_TTL":                "7s",
		EnvPrefix + "DNS_NEGATIVE_TTL":             "8s",
		EnvPrefix + "DNS_CACHE_SIZE":               "9",
		EnvPrefix + "IP_HEALTH_INTERVAL":           "10s",
		EnvPrefix + "IP_HEALTH_TIMEOUT":            "11s",
		EnvPrefix + "PACER_IDLE_TTL":               "12s",
		EnvPrefix + "PACER_MAX_TRACKED":            "13",
		EnvPrefix + "PACER_PROMOTE_AFTER":          "14",
		EnvPrefix + "PACER_SESSION_LEASE":          "45s",
		EnvPrefix + "PACER_LEASE_WAIT":             "40s",
		EnvPrefix + "PROBE_TIMEOUT":                "15s",
		EnvPrefix + "PROBE_MAX_RCPT_PER_SESSION":   "16",
		EnvPrefix + "PROBE_CATCH_ALL_PROBES":       "4",
		EnvPrefix + "PROBE_CATCH_ALL_AUDIT_RATE":   "0.25",
		EnvPrefix + "PROBE_STARTTLS":               "off",
		EnvPrefix + "PROBE_TLS_HANDSHAKE_TIMEOUT":  "7s",
		EnvPrefix + "PROBE_POLICY_STOP":            "6",
		EnvPrefix + "PROBE_POLICY_STOP_MAX":        "12",
		EnvPrefix + "PROBE_RANDOMISER_TTL":         "17s",
		EnvPrefix + "PROBE_DEFERRAL_RETRY":         "18s",
		EnvPrefix + "PROBE_MAX_EMAILS_PER_REQUEST": "19",
		EnvPrefix + "AUTH_ENABLED":                 "true",
		EnvPrefix + "SUPPRESS_ENABLED":             "1",
		EnvPrefix + "SUPPRESS_STALE":               "20s",
		EnvPrefix + "SUPPRESS_MAX_HASHES":          "21",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, ok := range map[string]bool{
		"http addr":     cfg.HTTP.Addr == "0.0.0.0:9",
		"tls":           cfg.TLS.MutualAuth() && cfg.TLS.Enabled(),
		"redis":         cfg.Redis.Addr == "127.0.0.1:6380" && cfg.Redis.DialTimeout == 5*time.Second,
		"dns servers":   len(cfg.DNS.Servers) == 2 && cfg.DNS.CacheSize == 9 && cfg.DNS.NegativeTTL == 8*time.Second,
		"ip health":     cfg.IPHealth.Enabled() && len(cfg.IPHealth.Zones) == 2 && cfg.IPHealth.Timeout == 11*time.Second,
		"suppress":      cfg.Suppress.Enabled && cfg.Suppress.Salt == "salt" && cfg.Suppress.MaxHashesPerImport == 21,
		"probe":         cfg.Probe.Port == "2525" && cfg.Probe.PolicyStopMax == 12 && cfg.Probe.MaxEmailsPerRequest == 19,
		"probe timings": cfg.Probe.RandomiserTTL == 17*time.Second && cfg.Probe.DeferralRetry == 18*time.Second,
		"catch-all":     cfg.Probe.CatchAllProbes == 4 && cfg.Probe.CatchAllAuditRate == 0.25,
		"starttls":      cfg.Probe.StartTLS == "off" && cfg.Probe.TLSHandshakeTimeout == 7*time.Second,
		"pacer":         cfg.Pacer.MaxTracked == 13 && cfg.Pacer.PromoteAfter == 14 && cfg.Pacer.IdleTTL == 12*time.Second,
		"pacer leases":  cfg.Pacer.SessionLease == 45*time.Second && cfg.Pacer.LeaseWait == 40*time.Second,
		"log":           cfg.Log.Level == "warn" && cfg.Log.Format == "text",
		"http timings":  cfg.HTTP.ShutdownTimeout == 4*time.Second && cfg.HTTP.IdleTimeout == 3*time.Second,
	} {
		if !ok {
			t.Errorf("%s: override not applied: %+v", name, cfg)
		}
	}
}

func TestEnvParseErrors(t *testing.T) {
	for key, value := range map[string]string{
		"DNS_TIMEOUT":                "soon",
		"PACER_MAX_TRACKED":          "many",
		"PROBE_POLICY_STOP":          "5.5",
		"PROBE_CATCH_ALL_AUDIT_RATE": "a twentieth",
		"AUTH_ENABLED":               "maybe",
		"SUPPRESS_ENABLED":           "yes please",
		"SUPPRESS_MAX_HASHES":        "1e9",
	} {
		t.Run(key, func(t *testing.T) {
			_, err := Load("", env(map[string]string{EnvPrefix + key: value}))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), EnvPrefix+key) {
				t.Errorf("Load with %s=%q: %v, want ErrInvalid naming the variable", key, value, err)
			}
		})
	}
}

func TestLoadFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(nil)); err == nil {
		t.Error("a missing file must be an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("http: [unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad, env(nil)); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Errorf("Load(bad yaml) = %v, want a parse error", err)
	}
	// A file that loads but does not validate is refused too.
	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("probe:\n  dial_network: tcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid, env(nil)); !errors.Is(err, ErrInvalid) {
		t.Errorf("Load(dial_network: tcp) = %v, want ErrInvalid", err)
	}
}
