package api

import (
	"io"
	"net/http"
	"time"
)

// Metrics is what the HTTP layer needs from the registry: somewhere to report
// how long a request took, and something to render on a scrape.
//
// Declared here, in the consumer (ENGINEERING-STANDARDS §2).
type Metrics interface {
	Observe(seconds float64)
	Render() string
}

func handleMetrics(m Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, m.Render())
	}
}

// timed records how long a handler took. It wraps rather than being folded into
// the handler so the measurement covers everything, including serialisation.
func timed(m Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		m.Observe(time.Since(start).Seconds())
	})
}
