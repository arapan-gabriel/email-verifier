package api

import (
	"context"
	"encoding/json"
	"net/http"
)

// Bands is the operator's view of what the pacer has learned, and the one
// action it cannot take for itself.
//
// Lowering a rate is reversible and belongs to the loop. **Raising a ceiling is
// not**: AIMD can undo a rate that turned out too high within a band, but it
// cannot undo a band that was widened wrongly, and the failure mode of a band
// that is too wide is a blocklisting rather than a slow run. So a proposal is
// evidence, and promoting it is a person's decision.
type Bands interface {
	Snapshot() []BandRow
	Promote(ctx context.Context, mxHost string) (any, error)
}

// BandRow is one tracked MX.
type BandRow struct {
	MXHost   string  `json:"mx_host"`
	Rate     float64 `json:"rate_per_sec"`
	MaxRate  float64 `json:"max_rate_per_sec"`
	State    string  `json:"state"`
	Proposal any     `json:"proposal,omitempty"`
}

func handleBands(b Bands) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rows := b.Snapshot()
		if rows == nil {
			rows = []BandRow{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"bands": rows})
	}
}

type promoteRequest struct {
	MXHost string `json:"mx_host"`
}

func handleBandPromote(b Bands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req promoteRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, "bad_request", "malformed JSON body: "+err.Error())
			return
		}
		if req.MXHost == "" {
			WriteError(w, http.StatusBadRequest, "bad_request", "mx_host is required")
			return
		}
		applied, err := b.Promote(r.Context(), req.MXHost)
		if err != nil {
			WriteError(w, http.StatusConflict, "no_proposal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"promoted": applied})
	}
}
