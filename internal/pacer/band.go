// Package pacer holds each recipient MX to a rate it tolerates.
//
// The model is AIMD over the shared token bucket: start at the band ceiling,
// halve on a genuine throttle, climb slowly while answers are clean, and pause
// the MX when the floor still is not enough. Only a real rate signal moves it —
// see Observe.
package pacer

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// seed holds the starting bands shipped with the binary, ported from
// ds-smtp-retry/config/limits-init. They are embedded rather than read from
// disk so the release artifact stays one file (ADR-005) and an operator cannot
// accidentally deploy a prober with no bands at all.
//
//go:embed bands/*.json
var seed embed.FS

// Band is a calibrated working range for one MX.
type Band struct {
	MinRate  float64 `json:"min_rate_per_sec"`
	MaxRate  float64 `json:"max_rate_per_sec"`
	MinConc  int     `json:"min_concurrency"`
	MaxConc  int     `json:"max_concurrency"`
	Burst    float64 `json:"burst"`
	Cooldown int     `json:"cooldown_seconds"`
	// Pause is how long a floored MX is left alone. The seed files spell it
	// recommended_pause_seconds; the Redis contract spells it pause_seconds.
	Pause    int `json:"pause_seconds"`
	PauseAlt int `json:"recommended_pause_seconds"`
}

// conservative is what an unknown MX gets: slow enough that being wrong about
// a stranger's tolerance costs latency rather than a blocklist entry.
func conservative() Band {
	return Band{MinRate: 0.1, MaxRate: 0.5, MinConc: 1, MaxConc: 1, Burst: 1, Cooldown: 300, Pause: 600}
}

func (b Band) normalise() Band {
	if b.Pause == 0 {
		b.Pause = b.PauseAlt
	}
	d := conservative()
	if b.MaxRate <= 0 {
		b.MaxRate = d.MaxRate
	}
	if b.MinRate <= 0 || b.MinRate > b.MaxRate {
		b.MinRate = min(d.MinRate, b.MaxRate)
	}
	if b.MinConc <= 0 {
		b.MinConc = 1
	}
	if b.MaxConc < b.MinConc {
		b.MaxConc = b.MinConc
	}
	if b.Burst <= 0 {
		b.Burst = 1
	}
	if b.Cooldown <= 0 {
		b.Cooldown = d.Cooldown
	}
	if b.Pause <= 0 {
		b.Pause = d.Pause
	}
	return b
}

// cooldown is how long a floored MX stays paused.
func (b Band) pauseFor() time.Duration { return time.Duration(b.Pause) * time.Second }

// Reader is the subset of the store the band lookup needs.
type Reader interface {
	Get(ctx context.Context, key string) (string, bool, error)
}

// bandFor resolves the working range for one MX, most specific first:
//
//  1. limits:mx:<host> in Redis — measured, and the only source allowed to
//     raise a ceiling (plan 012 writes it).
//  2. the shipped seed for the recipient domain — an educated guess.
//  3. the conservative default.
//
// A Redis failure here is not fatal on its own: the caller still has to take a
// token, and that call failing is what makes the probe fail closed.
func (p *Pacer) bandFor(ctx context.Context, mxHost, domain string) Band {
	if raw, ok, err := p.store.Get(ctx, "limits:mx:"+mxHost); err == nil && ok {
		var b Band
		if json.Unmarshal([]byte(raw), &b) == nil {
			return b.normalise()
		}
	}
	if b, ok := seedFor(domain); ok {
		return b
	}
	return conservative()
}

// seedFor finds the shipped band for a recipient domain, then for its
// registrable-looking parent, so mail.example.co.uk falls back to example.co.uk.
func seedFor(domain string) (Band, bool) {
	d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	for d != "" {
		if b, ok := readSeed(d); ok {
			return b, true
		}
		i := strings.Index(d, ".")
		if i < 0 {
			break
		}
		d = d[i+1:]
	}
	return Band{}, false
}

func readSeed(domain string) (Band, bool) {
	raw, err := seed.ReadFile(path.Join("bands", domain+".json"))
	if err != nil {
		return Band{}, false
	}
	var b Band
	if err := json.Unmarshal(raw, &b); err != nil {
		return Band{}, false
	}
	return b.normalise(), true
}

// SeedCount reports how many bands shipped with the binary; used by startup
// logging so a packaging mistake is visible rather than silent.
func SeedCount() int {
	entries, err := seed.ReadDir("bands")
	if err != nil {
		return 0
	}
	return len(entries)
}

func (b Band) String() string {
	return fmt.Sprintf("[%.3g..%.3g]/s burst %.3g pause %ds", b.MinRate, b.MaxRate, b.Burst, b.Pause)
}
