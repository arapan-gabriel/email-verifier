package admin_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/admin"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
)

func newReg(t *testing.T) (*httptest.Server, *policy.Engine, *clock.Offsetting) {
	t.Helper()
	p := &policy.Profile{Name: "gmail", Domains: []string{"gmail-sim.test"}, Listen: []string{"127.0.0.1:0"}}
	p.ApplyDefaults()
	clk := clock.New()
	eng := policy.NewEngine(p, clk)
	reg := &admin.Registry{
		Engines: map[string]*policy.Engine{"gmail": eng},
		Clock:   clk,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	return srv, eng, clk
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHealthzListsProfiles(t *testing.T) {
	srv, _, _ := newReg(t)
	status, body := do(t, "GET", srv.URL+"/healthz", "")
	if status != 200 || !strings.Contains(body, "gmail") {
		t.Fatalf("healthz: %d %s", status, body)
	}
}

func TestUnknownProfileIs404(t *testing.T) {
	srv, _, _ := newReg(t)
	if status, _ := do(t, "GET", srv.URL+"/stats?profile=nope", ""); status != 404 {
		t.Fatalf("want 404, got %d", status)
	}
}

func TestClockAdvanceMovesForwardOnly(t *testing.T) {
	srv, _, clk := newReg(t)

	status, body := do(t, "POST", srv.URL+"/clock/advance", `{"d":"5m"}`)
	if status != 200 {
		t.Fatalf("advance: %d %s", status, body)
	}
	if got := clk.Offset(); got != 5*time.Minute {
		t.Fatalf("offset: want 5m, got %s", got)
	}

	if status, _ := do(t, "POST", srv.URL+"/clock/advance", `{"d":"-5m"}`); status != 400 {
		t.Fatalf("negative advance should be rejected, got %d", status)
	}
	if got := clk.Offset(); got != 5*time.Minute {
		t.Fatalf("offset changed after a rejected advance: %s", got)
	}
}

func TestPutProfileHotSwapsLimits(t *testing.T) {
	srv, eng, _ := newReg(t)
	body := `{"name":"gmail","domains":["gmail-sim.test"],
	          "limits":{"rcpt_rate":{"count":7,"window":"30s"}}}`
	status, resp := do(t, "PUT", srv.URL+"/profiles/gmail", body)
	if status != 200 {
		t.Fatalf("put: %d %s", status, resp)
	}
	p := eng.Profile()
	if p.Limits.RcptRate.Count != 7 || p.Limits.RcptRate.Window.D() != 30*time.Second {
		t.Fatalf("limits not applied: %+v", p.Limits.RcptRate)
	}
	// Listen addresses are bound at startup and must survive a hot swap.
	if len(p.Listen) != 1 || p.Listen[0] != "127.0.0.1:0" {
		t.Fatalf("listen addresses lost: %v", p.Listen)
	}
}

func TestPutProfileRejectsTypos(t *testing.T) {
	srv, _, _ := newReg(t)
	// A typo in a limit name must fail loudly: silently ignoring it would
	// disable the very limit under test.
	status, body := do(t, "PUT", srv.URL+"/profiles/gmail", `{"name":"gmail","limits":{"rcpt_ratee":{"count":7}}}`)
	if status != 400 {
		t.Fatalf("want 400 for unknown field, got %d %s", status, body)
	}
	status, _ = do(t, "PUT", srv.URL+"/profiles/gmail", `{"name":"outlook"}`)
	if status != 400 {
		t.Fatalf("want 400 for name mismatch, got %d", status)
	}
}

func TestChaosPartialUpdate(t *testing.T) {
	srv, eng, _ := newReg(t)
	before := eng.Profile().Chaos.TempErrorReply

	if status, body := do(t, "POST", srv.URL+"/chaos", `{"profile":"gmail","temp_error_rate":0.25}`); status != 200 {
		t.Fatalf("chaos: %d %s", status, body)
	}
	c := eng.Profile().Chaos
	if c.TempErrorRate != 0.25 {
		t.Fatalf("temp_error_rate: %v", c.TempErrorRate)
	}
	if c.TempErrorReply != before {
		t.Fatalf("unset fields must be preserved, reply became %q", c.TempErrorReply)
	}
	if status, _ := do(t, "POST", srv.URL+"/chaos", `{"temp_error_rate":1.5}`); status != 400 {
		t.Fatal("out-of-range rate should be rejected")
	}
}

func TestResetClearsStats(t *testing.T) {
	srv, eng, _ := newReg(t)
	eng.OnRcpt("10.0.0.1", "valid@gmail-sim.test", 1)
	if eng.Stats().Rcpt == 0 {
		t.Fatal("precondition: expected a recorded RCPT")
	}
	if status, body := do(t, "POST", srv.URL+"/profiles/gmail/reset", ""); status != 200 {
		t.Fatalf("reset: %d %s", status, body)
	}
	if got := eng.Stats().Rcpt; got != 0 {
		t.Fatalf("rcpt after reset: %d", got)
	}
}

func TestStatsShapeIsStable(t *testing.T) {
	srv, eng, _ := newReg(t)
	eng.OnRcpt("10.0.0.1", "valid@gmail-sim.test", 1)
	_, body := do(t, "GET", srv.URL+"/stats?profile=gmail", "")
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("stats is not valid JSON: %v", err)
	}
	// These are the keys the test suite and the calibrator assert on.
	for _, k := range []string{"rcpt", "accepted", "rejected", "throttled",
		"max_concurrent_seen", "peak_rate_per_min", "code_counts", "clock_offset"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("stats is missing %q: %s", k, body)
		}
	}
}
