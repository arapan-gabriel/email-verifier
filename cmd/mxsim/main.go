// Command mxsim runs a set of deliberately badly-behaved SMTP servers, one per
// provider profile, so an email validator's RCPT TO logic and its adaptive
// per-MX rate limiter can be developed against known ground truth instead of
// against Gmail's patience.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/admin"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/metrics"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/smtp"
)

type kvFlag map[string]string

func (k kvFlag) String() string { return fmt.Sprint(map[string]string(k)) }

func (k kvFlag) Set(v string) error {
	name, addr, ok := strings.Cut(v, "=")
	if !ok || name == "" || addr == "" {
		return fmt.Errorf("expected name=addr, got %q", v)
	}
	k[name] = addr
	return nil
}

func main() {
	var (
		profilesDir = flag.String("profiles", "profiles", "directory of *.yaml profiles")
		adminAddr   = flag.String("admin", "127.0.0.1:8025", "control API listen address")
		metricsAddr = flag.String("metrics", "127.0.0.1:9101", "Prometheus metrics listen address")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error")
		only        = flag.String("only", "", "comma-separated profile names to run (default: all)")
		listenOver  = kvFlag{}
	)
	flag.Var(listenOver, "listen", "override a profile's listen address: -listen gmail=127.0.0.1:2525 (repeatable)")
	flag.Parse()

	log := newLogger(*logLevel)

	if err := run(*profilesDir, *adminAddr, *metricsAddr, *only, listenOver, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(profilesDir, adminAddr, metricsAddr, only string, listenOver kvFlag, log *slog.Logger) error {
	profs, err := policy.LoadDir(profilesDir)
	if err != nil {
		return err
	}
	if only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		kept := profs[:0]
		for _, p := range profs {
			if want[p.Name] {
				kept = append(kept, p)
				delete(want, p.Name)
			}
		}
		for n := range want {
			return fmt.Errorf("-only %s: no such profile", n)
		}
		profs = kept
	}

	clk := clock.New()
	engines := map[string]*policy.Engine{}
	servers := map[string]*smtp.Server{}

	for _, p := range profs {
		if addr, ok := listenOver[p.Name]; ok {
			p.Listen = strings.Split(addr, ",")
		}
		eng := policy.NewEngine(p, clk)
		engines[p.Name] = eng
		servers[p.Name] = smtp.New(eng, log)
	}
	for name := range listenOver {
		if engines[name] == nil {
			return fmt.Errorf("-listen %s=...: no such profile", name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var listeners []net.Listener
	fail := make(chan error, 1)

	for _, p := range profs {
		srv := servers[p.Name]
		for _, addr := range p.Listen {
			addr = strings.TrimSpace(addr)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				for _, l := range listeners {
					_ = l.Close()
				}
				return fmt.Errorf("profile %s: listen %s: %w", p.Name, addr, err)
			}
			listeners = append(listeners, ln)
			log.Info("smtp listening", "profile", p.Name, "addr", ln.Addr().String(),
				"domains", strings.Join(p.Domains, ","))
			wg.Add(1)
			go func(srv *smtp.Server, ln net.Listener) {
				defer wg.Done()
				if err := srv.Serve(ctx, ln); err != nil {
					select {
					case fail <- err:
					default:
					}
				}
			}(srv, ln)
		}
	}

	reg := &admin.Registry{Engines: engines, Clock: clk, Log: log}
	adminSrv := &http.Server{Addr: adminAddr, Handler: reg.Handler(), ReadHeaderTimeout: 5 * time.Second}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metrics.Handler(engines), ReadHeaderTimeout: 5 * time.Second}

	for _, s := range []*http.Server{adminSrv, metricsSrv} {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", s.Addr, err)
		}
		log.Info("http listening", "addr", ln.Addr().String())
		wg.Add(1)
		go func(s *http.Server, ln net.Listener) {
			defer wg.Done()
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case fail <- err:
				default:
				}
			}
		}(s, ln)
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-fail:
		log.Error("server failed", "err", err)
		stop()
		defer func() { _ = err }()
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
	for _, srv := range servers {
		srv.Shutdown()
	}
	wg.Wait()
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
