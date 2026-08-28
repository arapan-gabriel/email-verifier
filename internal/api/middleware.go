package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

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
