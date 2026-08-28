package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// Prober is the engine behind POST /probe.
//
// Declared here, in the package that calls it, holding the single method the
// handler needs (ENGINEERING-STANDARDS §2). A test satisfies it in three lines
// and never touches the network.
type Prober interface {
	Probe(ctx context.Context, req prober.Request) (prober.Response, error)
}

// maxBodyBytes caps a request body. The batch limit below is the real bound;
// this stops a malformed or hostile body before it is parsed at all.
const maxBodyBytes = 1 << 20

type probeRequest struct {
	MXHost       string   `json:"mx_host"`
	Domain       string   `json:"domain"`
	Emails       []string `json:"emails"`
	NeedCatchAll bool     `json:"need_catch_all"`
	Helo         string   `json:"helo,omitempty"`
	MailFrom     string   `json:"mail_from,omitempty"`
}

type probeResponse struct {
	SourceIP  string                   `json:"source_ip"`
	CheckedAt time.Time                `json:"checked_at"`
	Results   map[string]prober.Result `json:"results"`
}

func (r probeRequest) validate(maxEmails int) error {
	switch {
	case strings.TrimSpace(r.MXHost) == "":
		return errors.New("mx_host is required")
	case len(r.Emails) == 0:
		return errors.New("emails must not be empty")
	case len(r.Emails) > maxEmails:
		return errors.New("emails exceeds the per-request limit")
	case r.NeedCatchAll && strings.TrimSpace(r.Domain) == "":
		return errors.New("domain is required when need_catch_all is set")
	}
	for _, e := range r.Emails {
		if !strings.Contains(e, "@") {
			return errors.New("every entry in emails must be an address")
		}
	}
	return nil
}

// handleProbe asks one MX about a batch of addresses (ADR-006).
//
// The handler stays thin: parse, validate, one engine call, serialise. It maps
// no reply to a meaning — that lives in internal/prober and nowhere else
// (invariant 1).
func handleProbe(p Prober, sourceIP string, maxEmails int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req probeRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, "bad_request", "malformed JSON body: "+err.Error())
			return
		}
		if err := req.validate(maxEmails); err != nil {
			// A malformed request is a 400, never a verification result
			// (ENGINEERING-STANDARDS §4).
			WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		resp, err := p.Probe(r.Context(), prober.Request{
			MXHost:       req.MXHost,
			Domain:       req.Domain,
			Emails:       req.Emails,
			NeedCatchAll: req.NeedCatchAll,
			Helo:         req.Helo,
			MailFrom:     req.MailFrom,
		})
		if err != nil {
			WriteError(w, http.StatusBadGateway, "probe_failed", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, probeResponse{
			SourceIP:  sourceIP,
			CheckedAt: time.Now().UTC(),
			Results:   resp.Results,
		})
	}
}
