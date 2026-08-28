package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func routerWithLog(buf *bytes.Buffer, p Prober) http.Handler {
	return NewRouter(Options{
		Prober: p, MaxEmailsPerRequest: 10, SourceIP: "203.0.113.1",
		AuthEnabled: true, APIKey: "k",
		Logger: slog.New(slog.NewJSONHandler(buf, nil)),
	})
}

// A line nobody can join to the resolve, the pacing decision and the SMTP
// session it belongs to is a line nobody can follow.
func TestEveryRequestIsLoggedWithAnID(t *testing.T) {
	var buf bytes.Buffer
	h := routerWithLog(&buf, &fakeProber{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	id, _ := line["request_id"].(string)
	if id == "" {
		t.Error("no request_id")
	}
	if rec.Header().Get("X-Request-Id") != id {
		t.Error("the id in the response header does not match the log")
	}
	for _, k := range []string{"method", "path", "status", "duration_ms"} {
		if _, ok := line[k]; !ok {
			t.Errorf("log line has no %q", k)
		}
	}
}

// A caller-supplied id is honoured, so a trace spans both services.
func TestRequestIDIsCarriedFromTheCaller(t *testing.T) {
	var buf bytes.Buffer
	h := routerWithLog(&buf, &fakeProber{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "from-data-scout")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), "from-data-scout") {
		t.Errorf("the caller's id was discarded: %s", buf.String())
	}
}

// The local part is the customer's data and has no business in an operational
// log. The domain and a count are enough to find a problem.
func TestNoAddressIsLogged(t *testing.T) {
	var buf bytes.Buffer
	h := routerWithLog(&buf, &fakeProber{})

	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(
		`{"mx_host":"mx.test","domain":"example.test","emails":["secret.person@example.test"]}`))
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), "secret.person") {
		t.Errorf("an address reached the log:\n%s", buf.String())
	}
}
