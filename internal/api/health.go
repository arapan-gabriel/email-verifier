package api

import (
	"context"
	"net"
	"net/http"
	"time"
)

// ReadinessFunc reports whether the service can serve traffic. It returns nil
// when ready and an error describing what is missing otherwise.
type ReadinessFunc func(context.Context) error

// RedisReachable builds a readiness check that dials the operational store.
//
// Plan 003 replaces this with a real PING over the RESP client; until then a
// successful dial is the honest extent of what we can assert.
func RedisReachable(network, address string, timeout time.Duration) ReadinessFunc {
	return func(ctx context.Context) error {
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, network, address)
		if err != nil {
			return err
		}
		return conn.Close()
	}
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
