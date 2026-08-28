// Package api is the HTTP boundary: it parses, authenticates, calls one engine
// function, and serialises the result. No SMTP logic, no Redis access, and no
// reply classification lives here (ENGINEERING-STANDARDS §2).
package api

import "net/http"

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

	if opts.Prober != nil {
		limit := opts.MaxEmailsPerRequest
		if limit <= 0 {
			limit = 500
		}
		// Registered through Authenticated so the route cannot skip the guard.
		mux.Handle("POST /probe", opts.Authenticated(
			handleProbe(opts.Prober, opts.SourceIP, limit)))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return mux
}
