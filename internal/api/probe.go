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
	// PolicyStop raises or lowers, for this request only, how many consecutive
	// replies about our client end the session (plan 017). Omitted or zero uses
	// the configured default; the service clamps it, so an over-ambitious value
	// is quietly bounded rather than refused — a 400 here would reach the
	// caller as a transport failure and turn a whole batch into non-answers.
	PolicyStop int `json:"policy_stop,omitempty"`
	// Domains asks about several domains answered by this one MX (plan 033).
	// When present it replaces domain/emails/need_catch_all; sending both is
	// a 400. Whether the domains share a session is the service's decision.
	Domains []probeDomain `json:"domains,omitempty"`
}

type probeDomain struct {
	Domain       string   `json:"domain"`
	Emails       []string `json:"emails"`
	NeedCatchAll bool     `json:"need_catch_all"`
}

type probeResponse struct {
	SourceIP  string                   `json:"source_ip"`
	CheckedAt time.Time                `json:"checked_at"`
	Results   map[string]prober.Result `json:"results"`
}

func (r probeRequest) validate(maxEmails int) error {
	if r.Domains != nil {
		return r.validateDomains(maxEmails)
	}
	switch {
	case strings.TrimSpace(r.MXHost) == "":
		return errors.New("mx_host is required")
	case len(r.Emails) == 0:
		return errors.New("emails must not be empty")
	case len(r.Emails) > maxEmails:
		return errors.New("emails exceeds the per-request limit")
	case r.NeedCatchAll && strings.TrimSpace(r.Domain) == "":
		return errors.New("domain is required when need_catch_all is set")
	case r.PolicyStop < 0:
		return errors.New("policy_stop must not be negative")
	}
	for _, e := range r.Emails {
		if !strings.Contains(e, "@") {
			return errors.New("every entry in emails must be an address")
		}
	}
	return nil
}

// validateDomains checks the multi-domain shape (plan 033): exclusive with the
// single-domain fields, every group a named domain with its own addresses, and
// the request as a whole within the same per-request limit.
func (r probeRequest) validateDomains(maxEmails int) error {
	switch {
	case strings.TrimSpace(r.MXHost) == "":
		return errors.New("mx_host is required")
	case r.Domain != "" || r.Emails != nil || r.NeedCatchAll:
		return errors.New("domains replaces domain, emails and need_catch_all; send one shape")
	case len(r.Domains) == 0:
		return errors.New("domains must not be empty")
	case r.PolicyStop < 0:
		return errors.New("policy_stop must not be negative")
	}
	total := 0
	for _, d := range r.Domains {
		domain := strings.TrimSuffix(strings.TrimSpace(d.Domain), ".")
		if domain == "" {
			return errors.New("every entry in domains needs a domain")
		}
		if len(d.Emails) == 0 {
			return errors.New("every entry in domains needs emails")
		}
		for _, e := range d.Emails {
			_, at, ok := strings.Cut(e, "@")
			if !ok || !strings.EqualFold(strings.TrimSuffix(at, "."), domain) {
				return errors.New("every address in a domains entry must be at that domain")
			}
		}
		total += len(d.Emails)
	}
	if total > maxEmails {
		return errors.New("emails exceeds the per-request limit")
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

		preq := prober.Request{
			MXHost:       req.MXHost,
			Domain:       req.Domain,
			Emails:       req.Emails,
			NeedCatchAll: req.NeedCatchAll,
			Helo:         req.Helo,
			MailFrom:     req.MailFrom,
			PolicyStop:   req.PolicyStop,
		}
		for _, d := range req.Domains {
			preq.Domains = append(preq.Domains, prober.DomainGroup{
				Domain:       d.Domain,
				Emails:       d.Emails,
				NeedCatchAll: d.NeedCatchAll,
			})
		}
		resp, err := p.Probe(r.Context(), preq)
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
