// Package api is the HTTP boundary: it parses, authenticates, calls one engine
// function, and serialises the result. No SMTP logic, no Redis access, and no
// reply classification lives here (ENGINEERING-STANDARDS §2).
package api

import (
	"log/slog"
	"net/http"
)

// Options configures the router.
type Options struct {
	// Ready backs GET /readyz. Nil means "always ready".
	Ready ReadinessFunc
	// Prober backs POST /probe. Nil leaves the route unregistered.
	Prober Prober
	// SourceIP travels with every answer — a verdict is only as good as the IP
	// that produced it, and the caller stores it alongside the verdict.
	SourceIP string
	// MaxEmailsPerRequest bounds one batch.
	MaxEmailsPerRequest int
	// AuthEnabled turns on the credential check for non-health routes.
	AuthEnabled bool
	// APIKey is the expected bearer token when AuthEnabled is true.
	APIKey string
	// Metrics backs GET /metrics and times POST /probe. Nil leaves the route
	// unregistered and the timing unrecorded.
	Metrics Metrics
	// Logger receives one line per request. Nil discards them.
	Logger *slog.Logger
	// Health backs the operator's view of, and override on, the sending IP's
	// standing. Nil leaves those routes unregistered.
	Health HealthOverride
}

// Authenticated wraps h with the credential check required by invariant 11.
// Plan 001 registers POST /verify through this; health probes never go through
// it, and nothing else may skip it.
func (o Options) Authenticated(h http.Handler) http.Handler {
	if !o.AuthEnabled {
		return h
	}
	return requireAPIKey(o.APIKey, h)
}

// NewRouter builds the HTTP handler. GET /healthz and GET /readyz are the only
// unauthenticated routes and must stay that way (invariant 11).
func NewRouter(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("GET /readyz", handleReadyz(opts.Ready))

	if opts.Metrics != nil {
		// Operator surface, so it goes through the same guard as anything else
		// that is not a health probe (invariant 11).
		mux.Handle("GET /metrics", opts.Authenticated(handleMetrics(opts.Metrics)))
	}

	if opts.Health != nil {
		mux.Handle("GET /admin/ip-health", opts.Authenticated(handleIPHealth(opts.Health)))
		mux.Handle("POST /admin/ip-health/resume", opts.Authenticated(handleIPHealthResume(opts.Health)))
	}

	if opts.Prober != nil {
		limit := opts.MaxEmailsPerRequest
		if limit <= 0 {
			limit = 500
		}
		// Registered through Authenticated so the route cannot skip the guard.
		var probe http.Handler = handleProbe(opts.Prober, opts.SourceIP, limit)
		if opts.Metrics != nil {
			probe = timed(opts.Metrics, probe)
		}
		mux.Handle("POST /probe", opts.Authenticated(probe))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	if opts.Logger == nil {
		return mux
	}
	return withRequestID(opts.Logger, mux)
}
