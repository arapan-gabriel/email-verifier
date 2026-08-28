package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error is the single error shape the API returns (ENGINEERING-STANDARDS §4).
// A transport error is never confused with a verification verdict: a malformed
// request is a 400, not an "unknown".
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error Error `json:"error"`
}

// WriteError renders the canonical {"error":{"code","message"}} body.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already on the wire; all we can do is record it.
		slog.Error("write response", "error", err)
	}
}
