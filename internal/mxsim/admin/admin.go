// Package admin exposes the control plane: what the server saw, and how it
// should behave next. Tests assert against /stats rather than against
// client-side outcomes, and use /clock/advance so a 30-minute cooldown is
// testable in milliseconds.
package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/clock"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
)

type Registry struct {
	Engines map[string]*policy.Engine
	Clock   clock.Clock
	Log     *slog.Logger
}

func (r *Registry) names() []string {
	out := make([]string, 0, len(r.Engines))
	for n := range r.Engines {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "profiles": r.names()})
	})
	mux.HandleFunc("GET /profiles", r.listProfiles)
	mux.HandleFunc("GET /profiles/{name}", r.getProfile)
	mux.HandleFunc("PUT /profiles/{name}", r.putProfile)
	mux.HandleFunc("POST /profiles/{name}/reset", r.resetProfile)
	mux.HandleFunc("POST /reset", r.resetAll)
	mux.HandleFunc("GET /stats", r.stats)
	mux.HandleFunc("GET /transcript", r.transcript)
	mux.HandleFunc("POST /chaos", r.chaos)
	mux.HandleFunc("POST /clock/advance", r.advance)
	return logging(r.Log, mux)
}

func (r *Registry) listProfiles(w http.ResponseWriter, _ *http.Request) {
	out := make([]policy.Profile, 0, len(r.Engines))
	for _, n := range r.names() {
		out = append(out, r.Engines[n].Profile())
	}
	writeJSON(w, 200, out)
}

func (r *Registry) engine(w http.ResponseWriter, name string) (*policy.Engine, bool) {
	e := r.Engines[name]
	if e == nil {
		writeJSON(w, 404, map[string]any{"error": "unknown profile: " + name, "known": r.names()})
		return nil, false
	}
	return e, true
}

func (r *Registry) getProfile(w http.ResponseWriter, req *http.Request) {
	e, ok := r.engine(w, req.PathValue("name"))
	if !ok {
		return
	}
	writeJSON(w, 200, e.Profile())
}

func (r *Registry) putProfile(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	e, ok := r.engine(w, name)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	p, err := policy.ParseProfile(body, name)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if p.Name != name {
		writeJSON(w, 400, map[string]any{"error": "profile name in body does not match path"})
		return
	}
	// Listen addresses are bound at startup; keep the running ones.
	p.Listen = e.Profile().Listen
	if err := p.Validate(); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	e.SetProfile(p)
	writeJSON(w, 200, e.Profile())
}

func (r *Registry) resetProfile(w http.ResponseWriter, req *http.Request) {
	e, ok := r.engine(w, req.PathValue("name"))
	if !ok {
		return
	}
	e.Reset()
	writeJSON(w, 200, map[string]any{"reset": e.Name()})
}

func (r *Registry) resetAll(w http.ResponseWriter, _ *http.Request) {
	for _, e := range r.Engines {
		e.Reset()
	}
	writeJSON(w, 200, map[string]any{"reset": r.names()})
}

func (r *Registry) stats(w http.ResponseWriter, req *http.Request) {
	if name := req.URL.Query().Get("profile"); name != "" {
		e, ok := r.engine(w, name)
		if !ok {
			return
		}
		writeJSON(w, 200, e.Stats())
		return
	}
	out := map[string]policy.Stats{}
	for n, e := range r.Engines {
		out[n] = e.Stats()
	}
	writeJSON(w, 200, out)
}

func (r *Registry) transcript(w http.ResponseWriter, req *http.Request) {
	n := 100
	if v := req.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	name := req.URL.Query().Get("profile")
	if name == "" {
		writeJSON(w, 400, map[string]any{"error": "profile query parameter is required"})
		return
	}
	e, ok := r.engine(w, name)
	if !ok {
		return
	}
	writeJSON(w, 200, e.Transcript(n))
}

type chaosReq struct {
	Profile        string   `json:"profile"`
	TempErrorRate  *float64 `json:"temp_error_rate"`
	TempErrorReply *string  `json:"temp_error_reply"`
	DropRate       *float64 `json:"drop_rate"`
	Seed           *int64   `json:"seed"`
	TarpitBanner   *string  `json:"tarpit_banner"`
	TarpitRcpt     *string  `json:"tarpit_rcpt"`
}

func (r *Registry) chaos(w http.ResponseWriter, req *http.Request) {
	var body chaosReq
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	targets := r.names()
	if body.Profile != "" {
		if _, ok := r.engine(w, body.Profile); !ok {
			return
		}
		targets = []string{body.Profile}
	}
	for _, n := range targets {
		e := r.Engines[n]
		p := e.Profile()
		c := p.Chaos
		if body.TempErrorRate != nil {
			c.TempErrorRate = *body.TempErrorRate
		}
		if body.TempErrorReply != nil {
			c.TempErrorReply = *body.TempErrorReply
		}
		if body.DropRate != nil {
			c.DropRate = *body.DropRate
		}
		if body.Seed != nil {
			c.Seed = *body.Seed
		}
		if c.TempErrorRate < 0 || c.TempErrorRate > 1 || c.DropRate < 0 || c.DropRate > 1 {
			writeJSON(w, 400, map[string]any{"error": "rates must be within 0..1"})
			return
		}
		e.SetChaos(c)

		if body.TarpitBanner != nil || body.TarpitRcpt != nil {
			np := e.Profile()
			if body.TarpitBanner != nil {
				d, err := time.ParseDuration(*body.TarpitBanner)
				if err != nil {
					writeJSON(w, 400, map[string]any{"error": "tarpit_banner: " + err.Error()})
					return
				}
				np.Behaviour.TarpitBanner = policy.Duration(d)
			}
			if body.TarpitRcpt != nil {
				d, err := time.ParseDuration(*body.TarpitRcpt)
				if err != nil {
					writeJSON(w, 400, map[string]any{"error": "tarpit_rcpt: " + err.Error()})
					return
				}
				np.Behaviour.TarpitRcpt = policy.Duration(d)
			}
			e.SetProfile(&np)
		}
	}
	out := map[string]policy.Chaos{}
	for _, n := range targets {
		out[n] = r.Engines[n].Profile().Chaos
	}
	writeJSON(w, 200, out)
}

type advanceReq struct {
	// D is a Go duration string: "5m", "30s", "1h".
	D string `json:"d"`
}

func (r *Registry) advance(w http.ResponseWriter, req *http.Request) {
	var body advanceReq
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	d, err := time.ParseDuration(body.D)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "d: " + err.Error()})
		return
	}
	if d < 0 {
		writeJSON(w, 400, map[string]any{"error": "the clock only moves forward"})
		return
	}
	off := r.Clock.Advance(d)
	writeJSON(w, 200, map[string]any{"offset": off.String(), "now": r.Clock.Now()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Debug("admin", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
