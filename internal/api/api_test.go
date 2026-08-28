package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func do(t *testing.T, h http.Handler, method, target, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzIsUnauthenticatedAndOK(t *testing.T) {
	h := NewRouter(Options{AuthEnabled: true, APIKey: "k"})
	rec := do(t, h, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v, want status ok", body)
	}
}

func TestReadyz(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h := NewRouter(Options{Ready: func(context.Context) error { return nil }})
		if rec := do(t, h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("dependency down", func(t *testing.T) {
		h := NewRouter(Options{Ready: func(context.Context) error { return errors.New("redis down") }})
		rec := do(t, h, http.MethodGet, "/readyz", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})
}

func TestUnknownRouteIsAJSON404(t *testing.T) {
	rec := do(t, NewRouter(Options{}), http.MethodGet, "/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body struct{ Error Error }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want not_found", body.Error.Code)
	}
}

// A transport failure is never a verification verdict (ENGINEERING-STANDARDS §4).
func TestAuthenticatedRejectsBadCredentials(t *testing.T) {
	opts := Options{AuthEnabled: true, APIKey: "right-key"}
	guarded := opts.Authenticated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for name, header := range map[string]string{
		"missing":     "",
		"wrong key":   "Bearer wrong-key",
		"wrong shape": "Basic cm9vdDpyb290",
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, guarded, http.MethodGet, "/anything", header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body struct{ Error Error }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Code != "unauthorized" {
				t.Errorf("error.code = %q, want unauthorized", body.Error.Code)
			}
		})
	}

	if rec := do(t, guarded, http.MethodGet, "/anything", "Bearer right-key"); rec.Code != http.StatusOK {
		t.Errorf("valid key rejected: status = %d", rec.Code)
	}
}

func TestAuthenticatedIsPassThroughWhenDisabled(t *testing.T) {
	opts := Options{AuthEnabled: false}
	guarded := opts.Authenticated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	if rec := do(t, guarded, http.MethodGet, "/anything", ""); rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
}

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

// With no Redis there is no rate budget and every probe fails closed
// (invariant 5), so a node in that state must stop receiving work.
func TestStoreReachableReflectsThePing(t *testing.T) {
	if err := StoreReachable(stubPinger{})(context.Background()); err != nil {
		t.Errorf("healthy store reported not ready: %v", err)
	}
	down := errors.New("connection refused")
	if err := StoreReachable(stubPinger{err: down})(context.Background()); !errors.Is(err, down) {
		t.Errorf("error = %v, want the ping error", err)
	}
}
