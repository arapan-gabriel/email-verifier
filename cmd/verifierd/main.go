// Command verifierd is the isolated-IP mail edge: SMTP mailbox verification
// now, outbound relay in phase C. See docs/02-architecture/ARCHITECTURE.md.
package main

import (
	"context"
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
		logger.Warn("authentication disabled; only health endpoints may be exposed " +
			"(invariant 11 — real auth lands in plan 001)")
	}

	network, address := cfg.Redis.Endpoint()
	srv := &http.Server{
		Addr: cfg.HTTP.Addr,
		Handler: api.NewRouter(api.Options{
			Ready:       api.RedisReachable(network, address, cfg.Redis.DialTimeout),
			AuthEnabled: cfg.Auth.Enabled,
			APIKey:      cfg.Auth.APIKey,
		}),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTP.Addr, "redis", cfg.Redis.Addr)
		err := srv.ListenAndServe()
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
