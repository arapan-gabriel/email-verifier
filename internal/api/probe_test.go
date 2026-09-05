package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// fakeProber satisfies the one-method interface the handler declares, so no
// API test opens a socket (ENGINEERING-STANDARDS §2, §7).
type fakeProber struct {
	got prober.Request
	err error
}

func (f *fakeProber) Probe(_ context.Context, req prober.Request) (prober.Response, error) {
	f.got = req
	if f.err != nil {
		return prober.Response{}, f.err
	}
	yes, no := true, false
	out := prober.Response{Results: map[string]prober.Result{}}
	for i, e := range req.Emails {
		r := prober.Result{Connected: &yes, Class: prober.ClassValid, SMTPCode: 250}
		if i%2 == 1 {
			r = prober.Result{Connected: &yes, Accepted: &no, Class: prober.ClassInvalid, SMTPCode: 550}
		} else {
			r.Accepted = &yes
		}
		out.Results[e] = r
	}
	return out, nil
}

func postProbe(t *testing.T, h http.Handler, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func routerWith(p Prober) http.Handler {
	return NewRouter(Options{
		Prober: p, SourceIP: "92.222.87.97", MaxEmailsPerRequest: 3,
		AuthEnabled: true, APIKey: "right-key",
	})
}

const goodBody = `{"mx_host":"mx.test","domain":"example.test",
	"emails":["a@example.test","b@example.test"],"need_catch_all":true}`

func TestProbeReturnsPerAddressResults(t *testing.T) {
	f := &fakeProber{}
	rec := postProbe(t, routerWith(f), goodBody, "Bearer right-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got probeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SourceIP != "92.222.87.97" {
		t.Errorf("source_ip = %q — a verdict is only as good as the IP that produced it", got.SourceIP)
	}
	if len(got.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(got.Results))
	}
	if got.CheckedAt.IsZero() {
		t.Error("checked_at is zero")
	}
	// The request must reach the engine intact, grouping and all (ADR-006).
	if f.got.MXHost != "mx.test" || f.got.Domain != "example.test" || !f.got.NeedCatchAll {
		t.Errorf("engine received %+v", f.got)
	}
}

// Invariant 11: the route is registered through the guard, so it cannot be
// reached without credentials.
func TestProbeRequiresCredentials(t *testing.T) {
	for name, auth := range map[string]string{"missing": "", "wrong": "Bearer nope"} {
		t.Run(name, func(t *testing.T) {
			rec := postProbe(t, routerWith(&fakeProber{}), goodBody, auth)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// A malformed request is a 400, never a verification result
// (ENGINEERING-STANDARDS §4).
func TestProbeRejectsBadRequests(t *testing.T) {
	for name, body := range map[string]string{
		"not json":               `{`,
		"unknown field":          `{"mx_host":"mx.test","emails":["a@x.test"],"nonsense":1}`,
		"no mx_host":             `{"emails":["a@x.test"]}`,
		"empty emails":           `{"mx_host":"mx.test","emails":[]}`,
		"over the batch limit":   `{"mx_host":"mx.test","emails":["a@x.test","b@x.test","c@x.test","d@x.test"]}`,
		"not an address":         `{"mx_host":"mx.test","emails":["nonsense"]}`,
		"catch-all needs domain": `{"mx_host":"mx.test","emails":["a@x.test"],"need_catch_all":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postProbe(t, routerWith(&fakeProber{}), body, "Bearer right-key")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			var env struct{ Error Error }
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != "bad_request" {
				t.Errorf("error.code = %q, want bad_request", env.Error.Code)
			}
		})
	}
}

func TestProbeEngineFailureIsNotAVerdict(t *testing.T) {
	rec := postProbe(t, routerWith(&fakeProber{err: errors.New("boom")}), goodBody, "Bearer right-key")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	// The caller maps any transport-level failure to connected=false, never to
	// a verdict about a mailbox (ADR-006, invariant 1).
	if strings.Contains(rec.Body.String(), "results") {
		t.Error("a failed probe must not render a results object")
	}
}

func TestProbeUnregisteredWithoutAnEngine(t *testing.T) {
	h := NewRouter(Options{}) // no Prober
	rec := postProbe(t, h, goodBody, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

type fakeMetrics struct {
	observed []float64
	body     string
}

func (f *fakeMetrics) Observe(s float64) { f.observed = append(f.observed, s) }
func (f *fakeMetrics) Render() string    { return f.body }

// Operator surface, so it goes through the same guard as anything else that is
// not a health probe (invariant 11).
func TestMetricsRequiresCredentials(t *testing.T) {
	h := NewRouter(Options{
		Metrics:     &fakeMetrics{body: "verify_results_total{class=\"valid\"} 1\n"},
		AuthEnabled: true, APIKey: "right-key",
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer right-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want Prometheus text", ct)
	}
	if !strings.Contains(rec.Body.String(), "verify_results_total") {
		t.Error("body is not the rendered registry")
	}
}

func TestProbeIsTimed(t *testing.T) {
	m := &fakeMetrics{}
	h := NewRouter(Options{
		Prober: &fakeProber{}, MaxEmailsPerRequest: 10,
		AuthEnabled: true, APIKey: "right-key", Metrics: m,
	})
	if rec := postProbe(t, h, goodBody, "Bearer right-key"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(m.observed) != 1 {
		t.Fatalf("recorded %d durations, want 1", len(m.observed))
	}
	if m.observed[0] < 0 {
		t.Error("negative duration")
	}
}
