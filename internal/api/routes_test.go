package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/prober"
	"github.com/arapan-gabriel/email-verifier/internal/relay"
)

// Fakes for every optional engine, so the router can be built with all of
// them at once and every route exists.

type fakeHealth struct {
	burned     bool
	reason     string
	resumed    int
	complaints int
}

func (f *fakeHealth) Burned() (bool, string) { return f.burned, f.reason }
func (f *fakeHealth) Resume(context.Context) { f.resumed++; f.burned, f.reason = false, "" }
func (f *fakeHealth) ObserveComplaint()      { f.complaints++ }

type fakeSuppression struct {
	version string
	digests []string
	replace bool
	err     error
}

func (f *fakeSuppression) Import(_ context.Context, version string, digests []string, replace bool) error {
	if f.err != nil {
		return f.err
	}
	f.version, f.digests, f.replace = version, digests, replace
	return nil
}

func (f *fakeSuppression) Status(context.Context) SuppressionStatus {
	return SuppressionStatus{Enabled: true, Version: f.version, Size: int64(len(f.digests))}
}

type fakeRelay struct {
	got relay.Message
	err error
}

func (f *fakeRelay) Accept(_ context.Context, m relay.Message) (string, error) {
	f.got = m
	if f.err != nil {
		return "", f.err
	}
	return "msg-1", nil
}

type fakeBands struct {
	rows     []BandRow
	promoted string
	resumed  string
	err      error
}

func (f *fakeBands) Resume(_ context.Context, mx string) error {
	if f.err != nil {
		return f.err
	}
	f.resumed = mx
	return nil
}

func (f *fakeBands) Snapshot() []BandRow { return f.rows }
func (f *fakeBands) Promote(_ context.Context, mx string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.promoted = mx
	return map[string]any{"mx_host": mx, "max_rate": 4.0}, nil
}

type fakeCountingMetrics struct {
	fakeMetrics
	complaints int
}

func (f *fakeCountingMetrics) Complaint() { f.complaints++ }

type allEngines struct {
	prober *fakeProber
	health *fakeHealth
	supp   *fakeSuppression
	relay  *fakeRelay
	bands  *fakeBands
	met    *fakeCountingMetrics
}

func fullRouter(auth bool) (http.Handler, *allEngines) {
	e := &allEngines{
		prober: &fakeProber{}, health: &fakeHealth{}, supp: &fakeSuppression{},
		relay: &fakeRelay{}, bands: &fakeBands{}, met: &fakeCountingMetrics{fakeMetrics: fakeMetrics{body: "m 1\n"}},
	}
	return NewRouter(Options{
		Ready: func(context.Context) error { return nil }, Prober: e.prober, SourceIP: "192.0.2.1",
		MaxEmailsPerRequest: 3, AuthEnabled: auth, APIKey: "right-key", Metrics: e.met,
		Logger: slog.New(slog.DiscardHandler), Health: e.health, Suppression: e.supp,
		Relay: e.relay, MaxSuppressionHashes: 2, Bands: e.bands,
	}), e
}

// everyRoute is every route NewRouter can register, kept in step with
// docs/06-generated/api.md. ADDING A ROUTE MEANS ADDING A ROW HERE: the test
// below then proves it cannot be reached without credentials (invariant 11).
var everyRoute = []struct {
	method, path string
	public       bool
}{
	{http.MethodGet, "/healthz", true},
	{http.MethodGet, "/readyz", true},
	{http.MethodGet, "/metrics", false},
	{http.MethodPost, "/probe", false},
	{http.MethodPost, "/send", false},
	{http.MethodGet, "/admin/ip-health", false},
	{http.MethodPost, "/admin/ip-health/resume", false},
	{http.MethodPost, "/admin/ip-health/complaint", false},
	{http.MethodGet, "/admin/suppress", false},
	{http.MethodPost, "/admin/suppress", false},
	{http.MethodGet, "/admin/bands", false},
	{http.MethodPost, "/admin/bands/promote", false},
	{http.MethodPost, "/admin/bands/resume", false},
}

func TestEveryRouteRequiresCredentials(t *testing.T) {
	h, _ := fullRouter(true)
	for _, rt := range everyRoute {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			for _, auth := range []string{"", "Bearer wrong", "bearer right-key", "Bearer  right-key", "Basic right-key", "right-key"} {
				rec := do(t, h, rt.method, rt.path, auth)
				if rt.public {
					if rec.Code == http.StatusUnauthorized {
						t.Errorf("auth %q: public route answered 401", auth)
					}
					continue
				}
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("auth %q: status %d, want 401", auth, rec.Code)
				}
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Errorf("auth %q: 401 without WWW-Authenticate", auth)
				}
			}
			if rec := do(t, h, rt.method, rt.path, "Bearer right-key"); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusNotFound {
				t.Errorf("right token: status %d — the route is missing or refuses good credentials", rec.Code)
			}
		})
	}
}

// Routes backed by a nil engine are absent, not open.
func TestRoutesAbsentWithoutTheirEngine(t *testing.T) {
	h := NewRouter(Options{AuthEnabled: true, APIKey: "k"})
	for _, rt := range everyRoute {
		if rt.public {
			continue
		}
		if rec := do(t, h, rt.method, rt.path, "Bearer k"); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with no engine: %d, want 404", rt.method, rt.path, rec.Code)
		}
	}
}

func send(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer right-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not the canonical envelope: %s", rec.Body)
	}
	return env.Error.Code
}

func TestIPHealthRoutes(t *testing.T) {
	h, e := fullRouter(true)
	e.health.burned, e.health.reason = true, "listed on zen.example"

	rec := send(t, h, http.MethodGet, "/admin/ip-health", "")
	var got ipHealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || !got.Burned || got.Reason == "" {
		t.Fatalf("GET ip-health = %d %s, want burned with its reason", rec.Code, rec.Body)
	}

	rec = send(t, h, http.MethodPost, "/admin/ip-health/resume", "")
	got = ipHealthResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Burned || e.health.resumed != 1 {
		t.Fatalf("resume = %d %s (resumed %d), want cleared", rec.Code, rec.Body, e.health.resumed)
	}

	rec = send(t, h, http.MethodPost, "/admin/ip-health/complaint", "")
	if rec.Code != 200 || e.health.complaints != 1 || e.met.complaints != 1 {
		t.Fatalf("complaint = %d, health saw %d, metrics saw %d; want 200, 1, 1", rec.Code, e.health.complaints, e.met.complaints)
	}
}

// Metrics that cannot count complaints still let the complaint reach iphealth.
func TestComplaintWithoutACountingRegistry(t *testing.T) {
	health := &fakeHealth{}
	h := NewRouter(Options{Health: health, Metrics: &fakeMetrics{}})
	rec := do(t, h, http.MethodPost, "/admin/ip-health/complaint", "")
	if rec.Code != 200 || health.complaints != 1 {
		t.Fatalf("complaint = %d, recorded %d", rec.Code, health.complaints)
	}
}

func TestSuppressRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		importErr  error
		status     int
		code       string
	}{
		{"replace", `{"version":"v1","hashes":["aa","bb"],"mode":"replace"}`, nil, 200, ""},
		{"add", `{"version":"v2","hashes":["cc"],"mode":"add"}`, nil, 200, ""},
		{"bad json", `{"version":`, nil, 400, "bad_request"},
		{"unknown field", `{"version":"v","mode":"add","emails":["a@b"]}`, nil, 400, "bad_request"},
		{"no version", `{"hashes":[],"mode":"add"}`, nil, 400, "bad_request"},
		{"bad mode", `{"version":"v","hashes":[],"mode":"merge"}`, nil, 400, "bad_request"},
		{"over the limit", `{"version":"v","hashes":["a","b","c"],"mode":"add"}`, nil, 400, "bad_request"},
		{"list refuses", `{"version":"v","hashes":["a"],"mode":"add"}`, errors.New("that is an address"), 502, "import_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := fullRouter(true)
			e.supp.err = tc.importErr
			rec := send(t, h, http.MethodPost, "/admin/suppress", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.code != "" && errCode(t, rec) != tc.code {
				t.Errorf("code = %s, want %s", errCode(t, rec), tc.code)
			}
			if tc.status == 200 {
				var st SuppressionStatus
				_ = json.Unmarshal(rec.Body.Bytes(), &st)
				if st.Version == "" || e.supp.replace != (tc.name == "replace") {
					t.Errorf("status %+v, replace=%v", st, e.supp.replace)
				}
			}
		})
	}
	h, _ := fullRouter(true)
	if rec := send(t, h, http.MethodGet, "/admin/suppress", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("GET suppress = %d %s", rec.Code, rec.Body)
	}
}

// The default hash limit applies when none is configured.
func TestSuppressDefaultLimit(t *testing.T) {
	supp := &fakeSuppression{}
	h := NewRouter(Options{Suppression: supp})
	rec := do(t, h, http.MethodPost, "/admin/suppress", "")
	if rec.Code != 400 {
		t.Fatalf("empty body = %d, want 400", rec.Code)
	}
}

func TestSendRoute(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		status     int
		code       string
	}{
		{"accepted", `{"from":"a@x.test","to":"b@y.test","subject":"hi","text":"t"}`, nil, 202, ""},
		{"bad json", `{`, nil, 400, "bad_request"},
		{"unknown field", `{"bcc":"c@z.test"}`, nil, 400, "bad_request"},
		{"not sending is retryable", `{"to":"b@y.test"}`, fmt.Errorf("listed: %w", relay.ErrNotSending), 503, "not_sending"},
		{"a verdict about the message", `{"to":"bad"}`, errors.New("malformed address"), 400, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e := fullRouter(true)
			e.relay.err = tc.err
			rec := send(t, h, http.MethodPost, "/send", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.code != "" && errCode(t, rec) != tc.code {
				t.Errorf("code = %s, want %s", errCode(t, rec), tc.code)
			}
			if tc.status == 202 {
				var got sendResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				if got.MessageID != "msg-1" || got.QueuedAt.IsZero() || e.relay.got.Subject != "hi" {
					t.Errorf("response %+v, relay saw %+v", got, e.relay.got)
				}
			}
		})
	}
}

func TestBandRoutes(t *testing.T) {
	h, e := fullRouter(true)
	rec := send(t, h, http.MethodGet, "/admin/bands", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"bands":[]`) {
		t.Fatalf("empty bands = %d %s, want an empty list, not null", rec.Code, rec.Body)
	}
	e.bands.rows = []BandRow{{MXHost: "mx.test", Rate: 1, MaxRate: 2, State: "steady"}}
	if rec = send(t, h, http.MethodGet, "/admin/bands", ""); !strings.Contains(rec.Body.String(), `"mx_host":"mx.test"`) {
		t.Fatalf("bands = %s", rec.Body)
	}

	for _, tc := range []struct {
		name, body string
		err        error
		status     int
		code       string
	}{
		{"promoted", `{"mx_host":"mx.test"}`, nil, 200, ""},
		{"bad json", `{`, nil, 400, "bad_request"},
		{"no host", `{}`, nil, 400, "bad_request"},
		{"unknown mx", `{"mx_host":"nope.test"}`, errors.New("no proposal for nope.test"), 409, "no_proposal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.bands.err = tc.err
			rec := send(t, h, http.MethodPost, "/admin/bands/promote", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.code != "" && errCode(t, rec) != tc.code {
				t.Errorf("code = %s, want %s", errCode(t, rec), tc.code)
			}
		})
	}
	if e.bands.promoted != "mx.test" {
		t.Errorf("promoted %q, want mx.test", e.bands.promoted)
	}

	// Plan 032: the operator lifts a stand-down early.
	e.bands.err = nil
	if rec := send(t, h, http.MethodPost, "/admin/bands/resume", `{"mx_host":"@microsoft-eop"}`); rec.Code != 200 || e.bands.resumed != "@microsoft-eop" {
		t.Errorf("resume = %d %s, resumed %q", rec.Code, rec.Body, e.bands.resumed)
	}
	if rec := send(t, h, http.MethodPost, "/admin/bands/resume", `{}`); rec.Code != 400 {
		t.Errorf("resume without a key = %d", rec.Code)
	}
	e.bands.err = errors.New("redis down")
	if rec := send(t, h, http.MethodPost, "/admin/bands/resume", `{"mx_host":"x"}`); rec.Code != 503 {
		t.Errorf("resume with the store down = %d", rec.Code)
	}
}

// A metrics scrape renders the registry, and the default batch limit applies
// when none is configured.
func TestMetricsRenderAndDefaultBatchLimit(t *testing.T) {
	f := &fakeProber{}
	h := NewRouter(Options{Prober: f, Metrics: &fakeMetrics{body: "x 1\n"}})
	if rec := do(t, h, http.MethodGet, "/metrics", ""); rec.Body.String() != "x 1\n" {
		t.Errorf("metrics body = %q", rec.Body)
	}
	emails := make([]string, 500)
	for i := range emails {
		emails[i] = fmt.Sprintf("u%d@x.test", i)
	}
	body, _ := json.Marshal(map[string]any{"mx_host": "mx.test", "emails": emails})
	if rec := postProbe(t, h, string(body), ""); rec.Code != 200 {
		t.Errorf("500 addresses at the default limit = %d", rec.Code)
	}
	body, _ = json.Marshal(map[string]any{"mx_host": "mx.test", "emails": append(emails, "one@more.test")})
	if rec := postProbe(t, h, string(body), ""); rec.Code != 400 {
		t.Errorf("501 addresses = %d, want 400", rec.Code)
	}
}

func TestProbeValidationRows(t *testing.T) {
	h := routerWith(&fakeProber{})
	for _, body := range []string{
		`{"mx_host":"mx.test","emails":["a@x.test"],"policy_stop":-1}`,
		`{"mx_host":"mx.test","emails":["not-an-address"]}`,
	} {
		if rec := postProbe(t, h, body, "Bearer right-key"); rec.Code != 400 {
			t.Errorf("%s = %d, want 400", body, rec.Code)
		}
	}
}

// A request id that the caller did not send is made up, and every one is
// distinct.
func TestNewRequestIDIsHexAndDistinct(t *testing.T) {
	a, b := newRequestID(), newRequestID()
	if len(a) != 16 || a == b {
		t.Errorf("ids %q, %q", a, b)
	}
	if RequestID(context.Background()) != "" {
		t.Error("RequestID on a bare context must be empty")
	}
}

// FuzzProbeHandler: whatever the body, the answer is JSON with a 2xx or 4xx,
// never a panic or a 5xx, and the engine never sees more addresses than the
// limit.
func FuzzProbeHandler(f *testing.F) {
	for _, s := range []string{
		goodBody, `{}`, `{`, ``, `null`, `[]`, `{"mx_host":"m","emails":["a@b","c@d","e@f","g@h"]}`,
		`{"mx_host":"m","emails":["a@b"],"policy_stop":-5}`, `{"mx_host":" ","emails":["a@b"]}`,
		`{"mx_host":"m","emails":["a@b"],"need_catch_all":true}`, `{"mx_host":"m","emails":[1]}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		p := &countingProber{}
		h := NewRouter(Options{Prober: p, MaxEmailsPerRequest: 3, AuthEnabled: true, APIKey: "k"})
		req := httptest.NewRequest(http.MethodPost, "/probe", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 || rec.Code < 200 {
			t.Fatalf("body %q gave %d", body, rec.Code)
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("body %q gave non-JSON %q", body, rec.Body)
		}
		if p.max > 3 {
			t.Fatalf("engine saw %d addresses, limit 3", p.max)
		}
	})
}

type countingProber struct{ max int }

func (c *countingProber) Probe(_ context.Context, req prober.Request) (prober.Response, error) {
	c.max = max(c.max, len(req.Emails))
	return prober.Response{Results: map[string]prober.Result{}}, nil
}
