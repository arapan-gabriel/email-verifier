package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/prober"
)

// The multi-domain shape (plan 033) exactly as Data Scout's companion sends it
// reaches the engine as per-domain groups, and nothing of the old shape.
func TestProbeDomainsReachTheEngineAsGroups(t *testing.T) {
	f := &fakeProber{}
	body := `{"mx_host":"aspmx.l.google.com",
		"domains":[{"domain":"a.test","emails":["x@a.test"],"need_catch_all":true},
		           {"domain":"b.test","emails":["y@b.test","z@B.TEST"],"need_catch_all":false}]}`
	rec := postProbe(t, routerWith(f), body, "Bearer right-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	want := []prober.DomainGroup{
		{Domain: "a.test", Emails: []string{"x@a.test"}, NeedCatchAll: true},
		{Domain: "b.test", Emails: []string{"y@b.test", "z@B.TEST"}},
	}
	if !reflect.DeepEqual(f.got.Domains, want) {
		t.Errorf("engine got domains %+v, want %+v", f.got.Domains, want)
	}
	if f.got.MXHost != "aspmx.l.google.com" || f.got.Domain != "" || f.got.Emails != nil || f.got.NeedCatchAll {
		t.Errorf("engine got the old shape too: %+v", f.got)
	}
}

// The old shape is untouched: no Domains reach the engine.
func TestProbeOldShapeCarriesNoDomains(t *testing.T) {
	f := &fakeProber{}
	if rec := postProbe(t, routerWith(f), goodBody, "Bearer right-key"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f.got.Domains != nil {
		t.Errorf("old shape produced domains %+v", f.got.Domains)
	}
}

func TestProbeDomainsRejectsBadShapes(t *testing.T) {
	for name, body := range map[string]string{
		"both shapes, domain": `{"mx_host":"mx.test","domain":"a.test",` +
			`"domains":[{"domain":"a.test","emails":["x@a.test"]}]}`,
		"both shapes, emails": `{"mx_host":"mx.test","emails":["x@a.test"],` +
			`"domains":[{"domain":"a.test","emails":["x@a.test"]}]}`,
		"both shapes, need_catch_all": `{"mx_host":"mx.test","need_catch_all":true,` +
			`"domains":[{"domain":"a.test","emails":["x@a.test"]}]}`,
		"empty domains":     `{"mx_host":"mx.test","domains":[]}`,
		"no mx_host":        `{"domains":[{"domain":"a.test","emails":["x@a.test"]}]}`,
		"unnamed domain":    `{"mx_host":"mx.test","domains":[{"domain":" ","emails":["x@a.test"]}]}`,
		"domain sans email": `{"mx_host":"mx.test","domains":[{"domain":"a.test","emails":[]}]}`,
		"address elsewhere": `{"mx_host":"mx.test","domains":[{"domain":"a.test","emails":["x@b.test"]}]}`,
		"not an address":    `{"mx_host":"mx.test","domains":[{"domain":"a.test","emails":["nonsense"]}]}`,
		"negative stop":     `{"mx_host":"mx.test","policy_stop":-1,"domains":[{"domain":"a.test","emails":["x@a.test"]}]}`,
		"unknown inner key": `{"mx_host":"mx.test","domains":[{"domain":"a.test","emails":["x@a.test"],"x":1}]}`,
		"over the limit in total": `{"mx_host":"mx.test","domains":[{"domain":"a.test","emails":["p@a.test","q@a.test"]},` +
			`{"domain":"b.test","emails":["r@b.test","s@b.test"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postProbe(t, routerWith(&fakeProber{}), body, "Bearer right-key")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}
