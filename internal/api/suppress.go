package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// SuppressionAdmin is the push side of the suppression list.
//
// Pushed rather than pulled because Data Scout already calls this service:
// an endpoint here is less machinery than an endpoint there plus polling plus
// a second set of credentials pointing the other way.
type SuppressionAdmin interface {
	Import(ctx context.Context, version string, digests []string, replace bool) error
	Status(ctx context.Context) SuppressionStatus
}

// SuppressionStatus mirrors what the list reports, kept as its own type so the
// HTTP layer does not depend on the package's shape.
type SuppressionStatus struct {
	Enabled bool      `json:"enabled"`
	Version string    `json:"version,omitempty"`
	Updated time.Time `json:"updated_at,omitzero"`
	Size    int64     `json:"size"`
	Stale   bool      `json:"stale"`
}

type suppressImportRequest struct {
	Version string   `json:"version"`
	Hashes  []string `json:"hashes"`
	// Mode is "replace" for a full export or "add" for an increment. Replace is
	// what makes a removal at the source propagate.
	Mode string `json:"mode"`
}

func handleSuppressStatus(a SuppressionAdmin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Status(r.Context()))
	}
}

// handleSuppressImport accepts an export of **digests**. Addresses are refused
// by the list itself — sending one here would be the leak the hashing exists to
// prevent.
func handleSuppressImport(a SuppressionAdmin, maxHashes int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req suppressImportRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, "bad_request", "malformed JSON body: "+err.Error())
			return
		}
		switch {
		case req.Version == "":
			WriteError(w, http.StatusBadRequest, "bad_request", "version is required")
			return
		case req.Mode != "replace" && req.Mode != "add":
			WriteError(w, http.StatusBadRequest, "bad_request", `mode must be "replace" or "add"`)
			return
		case len(req.Hashes) > maxHashes:
			WriteError(w, http.StatusBadRequest, "bad_request", "hashes exceeds the per-request limit")
			return
		}

		if err := a.Import(r.Context(), req.Version, req.Hashes, req.Mode == "replace"); err != nil {
			WriteError(w, http.StatusBadGateway, "import_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.Status(r.Context()))
	}
}
