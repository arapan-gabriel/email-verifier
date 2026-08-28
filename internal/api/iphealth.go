package api

import (
	"context"
	"net/http"
)

// HealthOverride is the operator's way to clear a pause.
//
// It exists because the check can be wrong — a resolver that starts
// misbehaving, or a listing on a zone that turns out not to matter — and the
// cost of being wrong is the node answering nothing. Clearing it must not need
// a redeploy.
type HealthOverride interface {
	Burned() (bool, string)
	Resume(ctx context.Context)
}

type ipHealthResponse struct {
	Burned bool   `json:"burned"`
	Reason string `json:"reason,omitempty"`
}

func handleIPHealth(h HealthOverride) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		burned, reason := h.Burned()
		writeJSON(w, http.StatusOK, ipHealthResponse{Burned: burned, Reason: reason})
	}
}

// handleIPHealthResume clears the pause. The next scheduled check re-evaluates,
// so this overrides a verdict rather than disabling the checking.
func handleIPHealthResume(h HealthOverride) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.Resume(r.Context())
		burned, reason := h.Burned()
		writeJSON(w, http.StatusOK, ipHealthResponse{Burned: burned, Reason: reason})
	}
}
