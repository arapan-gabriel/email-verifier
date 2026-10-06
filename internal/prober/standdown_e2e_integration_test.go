package prober_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/limiter"
	"github.com/arapan-gabriel/email-verifier/internal/metrics"
	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
	"github.com/arapan-gabriel/email-verifier/internal/pacer"
	"github.com/arapan-gabriel/email-verifier/internal/prober"
	"github.com/arapan-gabriel/email-verifier/internal/redis"
	"github.com/arapan-gabriel/email-verifier/internal/standdown"
)

// Plan 032's manual-test gate, as a test that can be run again: real mxsim servers,
// the real prober, pacer, stand-down guard and metrics registry, wired the way
// cmd/verifierd wires them, over a real Redis. Only the resolver is the test's own
// (loopback, see loopbackResolver) — the binary's SSRF guard rightly refuses mxsim.
//
// Set STANDDOWN_E2E_METRICS_OUT to a path to keep the /metrics text rendered while
// the key is down; Data Scout's deploy/probe-standing.py parses that file by hand.

const (
	// Rule B: a refusal of us that names no list.
	unexplainedRefusal = "550 5.7.1 Service unavailable; client host rejected by policy"
	// Rule A: a refusal that names a blocklist.
	zenRefusal = "550 5.7.1 Service unavailable; Client host [192.0.2.1] blocked using zen.spamhaus.org"
)

type standDownRig struct {
	pace  *pacer.Pacer
	guard *standdown.Guard
	reg   *metrics.Registry
}

func newStandDownRig(t *testing.T, pause time.Duration, keys ...string) *standDownRig {
	t.Helper()
	addr := os.Getenv("VERIFIERD_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("VERIFIERD_TEST_REDIS_ADDR is not set")
	}
	store := redis.New(redis.Options{Addr: addr, Timeout: 2 * time.Second})
	clean := func() {
		for _, key := range keys {
			found, _ := store.Do(context.Background(), "KEYS", "*"+key+"*")
			if list, ok := found.([]any); ok {
				for _, k := range list {
					if s, ok := k.(string); ok {
						_, _ = store.Do(context.Background(), "DEL", s)
					}
				}
			}
		}
	}
	clean()
	t.Cleanup(func() { clean(); _ = store.Close() })

	reg := metrics.New(nil)
	pace := pacer.New(store, limiter.New(store), pacer.Options{
		Metrics:   reg,
		Leases:    limiter.NewLeases(store),
		LeaseTTL:  30 * time.Second,
		LeaseWait: 5 * time.Second,
	})
	reg.SetPacer(pace)
	guard := standdown.New(store, pace, reg, standdown.Options{
		Hosts: 3, Window: 30 * time.Minute, Pause: pause,
	})
	reg.SetStandDown(guard)
	return &standDownRig{pace: pace, guard: guard, reg: reg}
}

// probe asks mxHost (a family member's name, served by the mxsim on port) about
// one address and returns its result.
func (r *standDownRig) probe(t *testing.T, port, mxHost, domain string) prober.Result {
	t.Helper()
	p := prober.New(prober.Options{
		Pacer:       r.pace,
		Resolver:    loopbackResolver{},
		Helo:        "mail.datascoutmail.com",
		MailFrom:    "verify@probe.datascoutmail.com",
		Port:        port,
		DialNetwork: "tcp4",
		Timeout:     10 * time.Second,
		Metrics:     r.reg,
		OnRefusal: func(host, reply string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r.guard.Observe(ctx, host, reply)
		},
	})
	email := "someone@" + domain
	resp, err := p.Probe(t.Context(), prober.Request{MXHost: mxHost, Domain: domain, Emails: []string{email}})
	if err != nil {
		t.Fatalf("Probe %s: %v", mxHost, err)
	}
	return resp.Results[email]
}

// refusingMX runs an mxsim profile that refuses every recipient of its domains with
// reply — a receiver refusing *us*, not the address.
func refusingMX(t *testing.T, reply string, domains ...string) string {
	t.Helper()
	_, port, _ := startMX(t, "outlook", func(p *policy.Profile) {
		p.Domains = domains
		p.Recipients = policy.Recipients{}
		p.Behaviour.RejectUnknown = reply
	})
	return port
}

func stoodDownLine(reg *metrics.Registry, key string) string {
	for _, line := range strings.Split(reg.Render(), "\n") {
		if strings.HasPrefix(line, `verify_key_stood_down{mx_host="`+key+`"}`) {
			return line
		}
	}
	return ""
}

func TestStandDownEndToEndRuleBThreeHostsThenResume(t *testing.T) {
	const key = "@proofpoint"
	rig := newStandDownRig(t, 3*time.Second, key)
	hosts := []string{"mx0a-01.pphosted.com", "mx0b-02.pphosted.com", "mx0c-03.pphosted.com"}
	domains := []string{"one.pp-sim.test", "two.pp-sim.test", "three.pp-sim.test"}
	ports := make([]string, len(hosts))
	for i := range hosts {
		ports[i] = refusingMX(t, unexplainedRefusal, domains[i])
	}

	// Two hosts refuse us: evidence, not yet a pattern.
	for i := 0; i < 2; i++ {
		r := rig.probe(t, ports[i], hosts[i], domains[i])
		if r.Class != prober.ClassPolicy {
			t.Fatalf("host %d: class %q, want policy (reply %q)", i, r.Class, r.Reply)
		}
	}
	if line := stoodDownLine(rig.reg, key); line != "" && !strings.HasSuffix(line, " 0") {
		t.Fatalf("stood down after two hosts: %q", line)
	}

	// The third distinct host of the same system: rule B stands the key down.
	if r := rig.probe(t, ports[2], hosts[2], domains[2]); r.Class != prober.ClassPolicy {
		t.Fatalf("third host: class %q, want policy", r.Class)
	}
	if line := stoodDownLine(rig.reg, key); line != `verify_key_stood_down{mx_host="@proofpoint"} 1` {
		t.Fatalf("after three hosts the gauge reads %q, want 1", line)
	}
	if out := os.Getenv("STANDDOWN_E2E_METRICS_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(rig.reg.Render()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Any host of the system is now refused by us, before a socket opens: a
	// retry hint, never a verdict (us ≠ address).
	r := rig.probe(t, ports[0], hosts[0], domains[0])
	if r.Class != prober.ClassPaused || r.Accepted != nil || r.RetryAfterSeconds <= 0 {
		t.Fatalf("while down: class %q accepted %v retry %d, want paused / nil / >0",
			r.Class, r.Accepted, r.RetryAfterSeconds)
	}

	// pause_until passes; nobody resumes it.
	time.Sleep(3500 * time.Millisecond)
	if line := stoodDownLine(rig.reg, key); line != `verify_key_stood_down{mx_host="@proofpoint"} 0` {
		t.Fatalf("after the pause the gauge reads %q, want 0", line)
	}
	if r := rig.probe(t, ports[0], hosts[0], domains[0]); r.Class == prober.ClassPaused {
		t.Fatalf("still paused after pause_until without an operator: retry %d", r.RetryAfterSeconds)
	}
}

func TestStandDownEndToEndRuleANamedBlocklist(t *testing.T) {
	const key = "@mimecast"
	rig := newStandDownRig(t, time.Hour, key)
	port := refusingMX(t, zenRefusal, "one.mc-sim.test")

	if r := rig.probe(t, port, "de-smtp-inbound-1.mimecast.com", "one.mc-sim.test"); r.Class != prober.ClassPolicy {
		t.Fatalf("class %q, want policy (reply %q)", r.Class, r.Reply)
	}
	// One host naming a blocklist is enough.
	if line := stoodDownLine(rig.reg, key); line != `verify_key_stood_down{mx_host="@mimecast"} 1` {
		t.Fatalf("gauge %q, want 1 after one zen.spamhaus.org refusal", line)
	}
}
