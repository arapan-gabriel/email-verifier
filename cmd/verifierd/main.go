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
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/arapan-gabriel/email-verifier/internal/api"
	"github.com/arapan-gabriel/email-verifier/internal/config"
	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/metrics"
	"github.com/arapan-gabriel/email-verifier/internal/mxprofile"
	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
	"github.com/arapan-gabriel/email-verifier/internal/redis"
	"github.com/arapan-gabriel/email-verifier/internal/resolver"
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
	})
	reg.SetPacer(pace)

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
	})

	srv := &http.Server{
		Addr: cfg.HTTP.Addr,
		Handler: api.NewRouter(api.Options{
			Ready:               api.StoreReachable(store),
			Prober:              p,
			SourceIP:            cfg.Probe.SourceIP,
			MaxEmailsPerRequest: cfg.Probe.MaxEmailsPerRequest,
			AuthEnabled:         cfg.Auth.Enabled,
			APIKey:              cfg.Auth.APIKey,
			Metrics:             reg,
			Logger:              logger,
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

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.HTTP.Addr,
			"redis", cfg.Redis.Addr,
			"tls", cfg.TLS.Enabled(),
			"mtls", cfg.TLS.MutualAuth(),
			"helo", cfg.Probe.Helo,
			"source_ip", cfg.Probe.SourceIP,
			"seed_bands", pacer.SeedCount())
		var err error
		if cfg.TLS.Enabled() {
			err = srv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
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
