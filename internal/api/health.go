package api

import (
	"context"
	"net/http"
)

// ReadinessFunc reports whether the service can serve traffic. It returns nil
// when ready and an error describing what is missing otherwise.
type ReadinessFunc func(context.Context) error

// Pinger is the operational store, reduced to the one call readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StoreReachable builds a readiness check that PINGs the operational store.
//
// It matters more than a liveness check here: with no Redis there is no rate
// budget, and every probe fails closed (invariant 5). A node in that state
// should stop receiving work rather than answer "unattempted" to everything.
func StoreReachable(p Pinger) ReadinessFunc {
	return func(ctx context.Context) error { return p.Ping(ctx) }
}

// handleHealthz is liveness: the process is up and serving.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is readiness: dependencies are reachable. It returns 503 when
// they are not, so a load balancer stops sending work to a node that would
// only fail closed (invariant 5).
func handleReadyz(ready ReadinessFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		if err := ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not ready",
				"reason": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
