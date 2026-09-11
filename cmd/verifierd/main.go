// Command verifierd is the isolated-IP mail edge: SMTP mailbox verification
// now, outbound relay in phase C. See docs/02-architecture/ARCHITECTURE.md.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/arapan-gabriel/email-verifier/internal/api"
	"github.com/arapan-gabriel/email-verifier/internal/config"
	"github.com/arapan-gabriel/email-verifier/internal/iphealth"
	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/metrics"
	"github.com/arapan-gabriel/email-verifier/internal/mxprofile"
	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
	"github.com/arapan-gabriel/email-verifier/internal/redis"
	"github.com/arapan-gabriel/email-verifier/internal/relay"
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
	"github.com/arapan-gabriel/email-verifier/internal/suppress"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, os.Args, os.Getenv, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "verifierd: %v\n", err)
		os.Exit(1)
	}
}

// run is main's body with every ambient dependency passed in — arguments, the
// environment, the error stream, and the cancellation that stands in for a
// signal. That makes startup, shutdown and configuration failures testable
// without spawning a process (ENGINEERING-STANDARDS §2).
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to YAML config (optional; VERIFIERD_* env overrides)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath, getenv)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Log, stderr)

	if !cfg.Auth.Enabled {
		logger.Warn("authentication disabled — POST /probe is unguarded; " +
			"acceptable for local development only (invariant 11)")
	}
	if !cfg.TLS.Enabled() {
		logger.Warn("serving plain HTTP — the integration boundary is mTLS (ADR-006); " +
			"acceptable for local development only")
	} else if !cfg.TLS.MutualAuth() {
		logger.Warn("TLS without client certificates — set tls.client_ca_file for mTLS (ADR-006)")
	}

	store := redis.New(redis.Options{Addr: cfg.Redis.Addr, Timeout: cfg.Redis.DialTimeout})
	defer func() { _ = store.Close() }()

	reg := metrics.New(nil)
	pace := pacer.New(store, limiter.New(store), pacer.Options{
		IdleTTL:    cfg.Pacer.IdleTTL,
		MaxTracked: cfg.Pacer.MaxTracked,
		Metrics:    reg,
		Promote: pacer.Promotion{
			After:   cfg.Pacer.PromoteAfter,
			Step:    cfg.Pacer.PromoteStep,
			Ceiling: cfg.Pacer.PromoteCeiling,
		},
	})
	reg.SetPacer(pace)

	health := iphealth.New(iphealth.Options{
		IP:                 cfg.Probe.SourceIP,
		Zones:              cfg.IPHealth.Zones,
		Lookup:             dnsblLookup(cfg.IPHealth),
		Interval:           cfg.IPHealth.Interval,
		Store:              store,
		Metrics:            reg,
		ComplaintWindow:    cfg.IPHealth.ComplaintWindow,
		ComplaintThreshold: cfg.IPHealth.ComplaintThreshold,
	})
	if health.Enabled() {
		if err := health.SelfTest(ctx); err != nil {
			// A resolver we cannot trust disables the check. It must never
			// trigger one: a stub answers "listed" to every zone, and pausing
			// on that is an outage caused by a resolver misconfiguration.
			logger.Error("blocklist checking disabled — resolver failed its self-test", "error", err)
		} else {
			// The *effective* zones, not the configured ones: empty means "the
			// defaults", and a line reading zones=[] tells the reader nothing
			// about what is actually being checked.
			zones := cfg.IPHealth.Zones
			if len(zones) == 0 {
				zones = iphealth.DefaultZones
			}
			logger.Info("blocklist checking enabled", "zones", zones, "ip", cfg.Probe.SourceIP)
			go health.Run(ctx)
		}
	} else {
		logger.Warn("blocklist checking is off — set ip_health.resolvers to a resolver that can " +
			"answer DNSBL queries (the host's stub cannot)")
	}

	// Built whenever a salt is configured, not only when enforcement is on.
	// Those are different questions, and tying the list's existence to
	// enforcement made it impossible to load: `POST /admin/suppress` is
	// registered only when this is non-nil, so the first export had nowhere to
	// go until enforcement was already running — against a list nobody had
	// pushed, which is precisely what plan 011 warned against.
	var suppression *suppress.List
	if cfg.Suppress.Salt != "" {
		suppression = suppress.New(suppress.Options{
			Salt:    cfg.Suppress.Salt,
			Stale:   cfg.Suppress.Stale,
			Store:   store,
			Enforce: cfg.Suppress.Enabled,
		})
		st := suppression.Status(ctx)
		if cfg.Suppress.Enabled {
			logger.Info("suppression check enforced",
				"entries", st.Size, "version", st.Version, "stale", st.Stale)
		} else {
			logger.Info("suppression list loadable but not enforced — set suppress.enabled once a real export has landed",
				"entries", st.Size, "version", st.Version)
		}
	} else {
		logger.Info("local suppression check is off — Data Scout's is the only one")
	}

	dns := resolver.New(resolver.Options{
		Servers:     cfg.DNS.Servers,
		Timeout:     cfg.DNS.Timeout,
		CacheTTL:    cfg.DNS.CacheTTL,
		NegativeTTL: cfg.DNS.NegativeTTL,
		CacheSize:   cfg.DNS.CacheSize,
	})

	p := prober.New(prober.Options{
		Pacer:             pace,
		Resolver:          dns,
		Helo:              cfg.Probe.Helo,
		MailFrom:          cfg.Probe.MailFrom,
		Timeout:           cfg.Probe.Timeout,
		DialNetwork:       cfg.Probe.DialNetwork,
		Port:              cfg.Probe.Port,
		MaxRCPTPerSession: cfg.Probe.MaxRCPTPerSession,
		CatchAllProbes:    cfg.Probe.CatchAllProbes,
		PolicyStop:        cfg.Probe.PolicyStop,
		PolicyStopMax:     cfg.Probe.PolicyStopMax,
		DeferralRetry:     cfg.Probe.DeferralRetry,
		Profiles:          mxprofile.New(store, cfg.Probe.RandomiserTTL),
		Metrics:           reg,
		Health:            health,
		Suppress:          suppressionOrNil(suppression),
		OnSuppressionError: func(err error) {
			// Loud, but not fatal: this is a redundancy and the authoritative
			// check has already run upstream.
			logger.Error("suppression list unreadable; continuing on the caller's check", "error", err)
		},
		// Plan 018. Without this a `policy` verdict cannot be told from another
		// `policy` verdict an hour later, and Data Scout's warm-up ladder stops
		// on exactly that verdict moving. The reply is redacted inside the
		// prober, so nothing here can leak a recipient.
		ReplyMaxChars: cfg.Log.ReplyMaxChars,
		OnReply:       replyLogger(cfg.Log.Replies, logger),
	})

	// Outbound mail (plan 014). Nil unless configured, which leaves POST /send
	// unregistered: a node that only verifies should not be one DKIM key away
	// from being able to send.
	var outbound *relay.Relay
	if cfg.Relay.Enabled {
		key, err := os.ReadFile(cfg.Relay.DKIMKeyFile)
		if err != nil {
			return fmt.Errorf("relay: reading the DKIM key: %w", err)
		}
		signer, err := relay.NewSigner(cfg.Relay.Domain, cfg.Relay.DKIMSelector, key)
		if err != nil {
			return err
		}
		outbound = relay.New(relay.Options{
			Queue:    relay.NewQueue(store, cfg.Relay.MaxAttempts),
			Signer:   signer,
			Resolver: dns,
			Dialer:   &net.Dialer{Timeout: cfg.Relay.SendTimeout},
			Pacer:    pace,
			Suppress: relaySuppressionOrNil(suppression),
			Health:   health,
			Metrics:  reg,
			Helo:     cfg.Probe.Helo,
			ReturnPath: func(id string) string {
				return "bounces+" + id + "@" + cfg.Relay.ReturnPathDomain
			},
			Timeout:   cfg.Relay.SendTimeout,
			RetryBase: cfg.Relay.RetryBase,
			Network:   cfg.Probe.DialNetwork,
		})
		logger.Info("outbound relay enabled",
			"domain", cfg.Relay.Domain, "selector", cfg.Relay.DKIMSelector,
			"return_path", cfg.Relay.ReturnPathDomain)
		go drain(ctx, outbound, cfg.Relay.DrainInterval, logger)
	}

	srv := &http.Server{
		Addr: cfg.HTTP.Addr,
		Handler: api.NewRouter(api.Options{
			Ready:                api.StoreReachable(store),
			Prober:               p,
			SourceIP:             cfg.Probe.SourceIP,
			MaxEmailsPerRequest:  cfg.Probe.MaxEmailsPerRequest,
			AuthEnabled:          cfg.Auth.Enabled,
			APIKey:               cfg.Auth.APIKey,
			Metrics:              reg,
			Logger:               logger,
			Health:               health,
			Suppression:          suppressionAdminOrNil(suppression),
			MaxSuppressionHashes: cfg.Suppress.MaxHashesPerImport,
			Bands:                bandsView{pace},
			Relay:                relayOrNil(outbound),
		}),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	tlsCfg, err := clientAuthTLS(cfg.TLS)
	if err != nil {
		return err
	}
	srv.TLSConfig = tlsCfg

	// Bind before announcing anything. ListenAndServe binds inside the
	// goroutine, so a failure there — "address already in use" above all —
	// surfaces asynchronously, after we have already logged that we are
	// listening. Binding here makes that a plain startup error, and gives us
	// the one moment at which READY=1 is true.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTP.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLS.Enabled() {
			err = srv.ServeTLS(ln, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = srv.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	logger.Info("listening",
		"addr", cfg.HTTP.Addr,
		"redis", cfg.Redis.Addr,
		"tls", cfg.TLS.Enabled(),
		"mtls", cfg.TLS.MutualAuth(),
		"helo", cfg.Probe.Helo,
		// The envelope sender is deployed identity, same as the HELO name and
		// the egress address, and it decides whose reputation a probe spends
		// (plan 019). Without it here, "which sender is live" is answerable
		// only by reading the file on the host.
		"mail_from", cfg.Probe.MailFrom,
		"source_ip", cfg.Probe.SourceIP,
		"seed_bands", pacer.SeedCount())
	sdNotify("READY=1")

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		// Tell systemd the drain has begun, so TimeoutStopSec is measured
		// against a shutdown we acknowledged rather than a process that went
		// quiet.
		sdNotify("STOPPING=1")
		logger.Info("shutdown signal received", "drain_timeout", cfg.HTTP.ShutdownTimeout)
	}

	// A fresh context: ctx is already cancelled, and the drain is exactly what
	// must still be allowed to run. systemd's TimeoutStopSec covers this window
	// so in-flight SMTP dialogues are not cut mid-session.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("stopped cleanly")
	return <-serveErr
}

// replyLogger returns the hook that writes what a server said, or nil when the
// operator has turned it off. Returning nil rather than a no-op function keeps
// the check on the prober's side, where it is one nil test per result instead
// of a call per result.
func replyLogger(enabled bool, logger *slog.Logger) func(prober.ReplyEvent) {
	if !enabled {
		return nil
	}
	return func(ev prober.ReplyEvent) {
		logger.Info("smtp_reply",
			"mx_host", ev.MXHost,
			"class", string(ev.Class),
			"smtp_code", ev.SMTPCode,
			"enhanced_code", ev.EnhancedCode,
			"reply", ev.Reply,
			"err", ev.Err)
	}
}

// drain empties the outbound queue, one message at a time.
//
// One at a time on purpose: the pacer already decides how fast this IP may talk
// to any given MX, and a pool of senders would race it rather than obey it. The
// interval is how long to wait after finding nothing — a queue with work in it
// loops without sleeping.
func drain(ctx context.Context, r *relay.Relay, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		did, err := r.DeliverNext(ctx)
		switch {
		case errors.Is(err, relay.ErrNotSending):
			// A listed IP, or a suppression list we cannot vouch for. Not an
			// error to log every ten seconds — it is a state, and the metric
			// and the health endpoint already say so.
		case err != nil:
			logger.Error("relay drain", "error", err)
		}
		if did && err == nil {
			continue // there may be more, and the pacer sets the pace
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// relayOrNil keeps a typed nil out of the router's interface field, where it
// would read as "configured" and register a route that panics on first use.
func relayOrNil(r *relay.Relay) api.Relay {
	if r == nil {
		return nil
	}
	return r
}

func relaySuppressionOrNil(l *suppress.List) relay.Suppression {
	if l == nil {
		return nil
	}
	return l
}

// clientAuthTLS builds the listener's TLS configuration. When a client CA is
// configured the handshake requires and verifies a client certificate, so a
// scanner is turned away before its request reaches any handler (ADR-006).
func clientAuthTLS(cfg config.TLS) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil //nolint:nilnil // no TLS configured is a valid state, not an error
	}
	out := &tls.Config{MinVersion: tls.VersionTLS13}
	if !cfg.MutualAuth() {
		return out, nil
	}
	pem, err := os.ReadFile(cfg.ClientCAFile) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read tls.client_ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls.client_ca_file %s contains no usable certificate", cfg.ClientCAFile)
	}
	out.ClientCAs = pool
	out.ClientAuth = tls.RequireAndVerifyClientCert
	return out, nil
}

// dnsblLookup builds the query function, or nil when no resolver is configured
// — which is what keeps checking off rather than falling back to the host's.
func dnsblLookup(cfg config.IPHealth) iphealth.LookupFunc {
	if !cfg.Enabled() {
		return nil
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: cfg.Timeout}
			var lastErr error
			for _, s := range cfg.Resolvers {
				c, err := d.DialContext(ctx, network, s)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		return r.LookupNetIP(ctx, "ip4", host)
	}
}

// A typed nil in an interface is not nil, so both wrappers return an untyped
// nil when suppression is off. Getting this wrong would turn "disabled" into a
// panic on the first probe.
func suppressionOrNil(l *suppress.List) prober.Suppression {
	if l == nil {
		return nil
	}
	return l
}

func suppressionAdminOrNil(l *suppress.List) api.SuppressionAdmin {
	if l == nil {
		return nil
	}
	return suppressAdmin{l}
}

// suppressAdmin adapts the list to what the HTTP layer reports, so neither
// package depends on the other's shape.
type suppressAdmin struct{ *suppress.List }

func (s suppressAdmin) Status(ctx context.Context) api.SuppressionStatus {
	st := s.List.Status(ctx)
	return api.SuppressionStatus{
		Enabled: st.Enabled, Version: st.Version, Updated: st.Updated,
		Size: st.Size, Stale: st.Stale,
	}
}

// bandsView adapts the pacer to the operator's band view, so neither package
// depends on the other's shape.
type bandsView struct{ p *pacer.Pacer }

func (b bandsView) Snapshot() []api.BandRow {
	snap := b.p.Snapshot()
	rows := make([]api.BandRow, 0, len(snap))
	for _, s := range snap {
		row := api.BandRow{MXHost: s.Host, Rate: s.Rate, MaxRate: s.MaxRate, State: s.State}
		if pr, ok := b.p.Proposal(context.Background(), s.Host); ok {
			row.Proposal = pr
		}
		rows = append(rows, row)
	}
	return rows
}

func (b bandsView) Promote(ctx context.Context, mxHost string) (any, error) {
	return b.p.Promote(ctx, mxHost)
}

func newLogger(cfg config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
