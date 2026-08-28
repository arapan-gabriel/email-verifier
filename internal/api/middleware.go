package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the id carried by this request's context, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID stamps every request with an id and logs its outcome.
//
// The id is what makes a line in the log joinable to the resolve, the pacing
// decision and the SMTP session it belongs to. A request without one is a line
// nobody can follow.
//
// **No address is logged.** The domain and a count are enough to find a
// problem; the local part is the customer's data and has no business in an
// operational log.
func withRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-Id", id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		logger.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// requireAPIKey guards every route except the health probes (invariant 11).
//
// This is the plan-000 stub: a constant-time bearer-token comparison, enough to
// keep an unauthenticated surface from existing the moment the first real
// endpoint lands. Plan 001 replaces it with mTLS or a managed key.
func requireAPIKey(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(key)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="verifierd"`)
			WriteError(w, http.StatusUnauthorized, "unauthorized", "valid credentials are required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
