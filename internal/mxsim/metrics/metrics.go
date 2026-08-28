// Package metrics renders Prometheus text exposition by hand. The client
// library would be a heavier dependency than the whole simulator, and the
// metric names here deliberately mirror the ones the validator exports so a
// dashboard built against the lab works against production.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
)

func Handler(engines map[string]*policy.Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		names := make([]string, 0, len(engines))
		for n := range engines {
			names = append(names, n)
		}
		sort.Strings(names)

		var b strings.Builder
		h := func(name, typ, help string) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}

		h("mxsim_conns_total", "counter", "Connections accepted by the simulator.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_conns_total{profile=%q} %d\n", n, engines[n].Stats().Conns)
		}
		h("mxsim_conns_rejected_total", "counter", "Connections refused by policy.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_conns_rejected_total{profile=%q} %d\n", n, engines[n].Stats().ConnsRejected)
		}
		h("mxsim_conns_active", "gauge", "Currently open connections.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_conns_active{profile=%q} %d\n", n, engines[n].Stats().CurrentConcurrent)
		}
		h("mxsim_conns_active_max", "gauge", "High-water mark of concurrent connections.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_conns_active_max{profile=%q} %d\n", n, engines[n].Stats().MaxConcurrentSeen)
		}
		h("mxsim_rcpt_total", "counter", "RCPT TO commands by reply code.")
		for _, n := range names {
			st := engines[n].Stats()
			codes := make([]string, 0, len(st.CodeCounts))
			for c := range st.CodeCounts {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			for _, c := range codes {
				fmt.Fprintf(&b, "mxsim_rcpt_total{profile=%q,code=%q} %d\n", n, c, st.CodeCounts[c])
			}
		}
		h("mxsim_throttle_total", "counter", "Throttling events (421/cooldown).")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_throttle_total{profile=%q} %d\n", n, engines[n].Stats().Throttled)
		}
		h("mxsim_cooldowns_total", "counter", "Cooldown periods started.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_cooldowns_total{profile=%q} %d\n", n, engines[n].Stats().Cooldowns)
		}
		h("mxsim_rcpt_peak_per_min", "gauge", "Peak observed RCPT rate per minute, per profile.")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_rcpt_peak_per_min{profile=%q} %d\n", n, engines[n].Stats().PeakRatePerMin)
		}
		h("mxsim_bad_sequence_total", "counter", "Out-of-sequence commands (client bugs).")
		for _, n := range names {
			fmt.Fprintf(&b, "mxsim_bad_sequence_total{profile=%q} %d\n", n, engines[n].Stats().BadSequence)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}
