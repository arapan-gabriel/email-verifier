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
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
	"github.com/arapan-gabriel/email-verifier/internal/suppress"
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
		IP:       cfg.Probe.SourceIP,
		Zones:    cfg.IPHealth.Zones,
		Lookup:   dnsblLookup(cfg.IPHealth),
		Interval: cfg.IPHealth.Interval,
		Store:    store,
		Metrics:  reg,
	})
	if health.Enabled() {
		if err := health.SelfTest(ctx); err != nil {
			// A resolver we cannot trust disables the check. It must never
			// trigger one: a stub answers "listed" to every zone, and pausing
			// on that is an outage caused by a resolver misconfiguration.
			logger.Error("blocklist checking disabled — resolver failed its self-test", "error", err)
		} else {
			logger.Info("blocklist checking enabled", "zones", cfg.IPHealth.Zones, "ip", cfg.Probe.SourceIP)
			go health.Run(ctx)
		}
	} else {
		logger.Warn("blocklist checking is off — set ip_health.resolvers to a resolver that can " +
			"answer DNSBL queries (the host's stub cannot)")
	}

	var suppression *suppress.List
	if cfg.Suppress.Enabled {
		suppression = suppress.New(suppress.Options{
			Salt: cfg.Suppress.Salt, Stale: cfg.Suppress.Stale, Store: store,
		})
		st := suppression.Status(ctx)
		logger.Info("suppression check enabled", "entries", st.Size, "version", st.Version, "stale", st.Stale)
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
	})

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
