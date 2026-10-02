package mxprofile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	kv      map[string]string
	getErr  error
	doErr   error
	lastCmd []string
}

func (f *fakeStore) Get(_ context.Context, k string) (string, bool, error) {
	if f.getErr != nil {
		return "", false, f.getErr
	}
	v, ok := f.kv[k]
	return v, ok, nil
}

func (f *fakeStore) Do(_ context.Context, args ...string) (any, error) {
	f.lastCmd = args
	if f.doErr != nil {
		return nil, f.doErr
	}
	if len(args) >= 3 && args[0] == "SET" {
		f.kv[args[1]] = args[2]
	}
	return "OK", nil
}

func newFake() *fakeStore { return &fakeStore{kv: map[string]string{}} }

func TestMarkThenRead(t *testing.T) {
	f := newFake()
	p := New(f, time.Hour)
	ctx := t.Context()

	if p.IsRandomiser(ctx, "mx.test") {
		t.Fatal("an unknown host reported as a randomiser")
	}
	p.MarkRandomiser(ctx, "mx.test")
	if !p.IsRandomiser(ctx, "mx.test") {
		t.Error("the verdict was not remembered")
	}
	// The verdict is about this server and nothing else.
	if p.IsRandomiser(ctx, "other.test") {
		t.Error("the verdict leaked to a different host")
	}
}

// It expires, so a server that stops randomising is eventually re-examined.
func TestMarkSetsATTL(t *testing.T) {
	f := newFake()
	New(f, 90*time.Minute).MarkRandomiser(t.Context(), "mx.test")
	cmd := strings.Join(f.lastCmd, " ")
	if !strings.Contains(cmd, "EX 5400") {
		t.Errorf("command = %q, want an EX of 5400 seconds", cmd)
	}
	if f.lastCmd[1] != "mx:mx.test:randomiser" {
		t.Errorf("key = %q", f.lastCmd[1])
	}
}

// Unlike the rate budget, not knowing this costs accuracy, not safety: the
// caller simply probes again. Failing the request would trade a real answer for
// no answer.
func TestStoreFailureDegradesToProbeAgain(t *testing.T) {
	f := newFake()
	f.getErr = errors.New("connection refused")
	if New(f, time.Hour).IsRandomiser(t.Context(), "mx.test") {
		t.Error("a store failure was read as a randomiser verdict")
	}

	f2 := newFake()
	f2.doErr = errors.New("connection refused")
	New(f2, time.Hour).MarkRandomiser(t.Context(), "mx.test") // must not panic
}

func TestNilAndEmptyAreSafe(t *testing.T) {
	var p *Profiles
	if p.IsRandomiser(t.Context(), "mx.test") {
		t.Error("a nil Profiles reported a verdict")
	}
	p.MarkRandomiser(t.Context(), "mx.test")

	live := New(newFake(), time.Hour)
	if live.IsRandomiser(t.Context(), "") {
		t.Error("an empty host reported a verdict")
	}
	live.MarkRandomiser(t.Context(), "")
}

func TestDefaultTTL(t *testing.T) {
	f := newFake()
	New(f, 0).MarkRandomiser(t.Context(), "mx.test")
	if !strings.Contains(strings.Join(f.lastCmd, " "), "EX 86400") {
		t.Errorf("default TTL = %v, want one day", f.lastCmd)
	}
}

func TestKey(t *testing.T) {
	if got := Key("gmail-smtp-in.l.google.com"); got != "mx:gmail-smtp-in.l.google.com:randomiser" {
		t.Errorf("Key = %q", got)
	}
}

// Plan 026 shares one pace bucket across a family's tenants; the randomiser
// verdict deliberately stays per host. Whether EOP answers by coin flip is a
// property of one tenant's configuration, and condemning every tenant for one
// would turn thousands of real verdicts into catch-all.
func TestRandomiserStaysPerTenant(t *testing.T) {
	f := newFake()
	p := New(f, time.Hour)
	ctx := t.Context()
	p.MarkRandomiser(ctx, "contoso-com.mail.protection.outlook.com")
	if p.IsRandomiser(ctx, "fabrikam-de.mail.protection.outlook.com") {
		t.Error("marking one tenant condemned a sibling tenant")
	}
	for k := range f.kv {
		if strings.Contains(k, "@") {
			t.Errorf("a randomiser verdict was stored under a family key: %q", k)
		}
	}
}

// A failed STARTTLS handshake is remembered per host, with the profile TTL,
// under its own key (plan 031).
func TestTLSBrokenIsRememberedPerHost(t *testing.T) {
	f := newFake()
	p := New(f, 90*time.Minute)
	ctx := t.Context()
	if p.TLSBroken(ctx, "mx1.jimdo.com") {
		t.Fatal("an unknown host reported as TLS-broken")
	}
	p.MarkTLSBroken(ctx, "mx1.jimdo.com")
	if got := strings.Join(f.lastCmd, " "); got != "SET mx:mx1.jimdo.com:tls_broken 1 EX 5400" {
		t.Errorf("command = %q", got)
	}
	if !p.TLSBroken(ctx, "mx1.jimdo.com") {
		t.Error("the failed handshake was not remembered")
	}
	if p.TLSBroken(ctx, "mx2.jimdo.com") {
		t.Error("the memory leaked to a different host")
	}
	if p.IsRandomiser(ctx, "mx1.jimdo.com") {
		t.Error("TLS-broken read as a randomiser verdict")
	}
}

// A store failure reads as "not broken": the session tries TLS again and falls
// back if it must. Nil and empty are safe.
func TestTLSBrokenFailsOpen(t *testing.T) {
	f := newFake()
	p := New(f, time.Hour)
	p.MarkTLSBroken(t.Context(), "mx.test")
	f.getErr = errors.New("redis down")
	if p.TLSBroken(t.Context(), "mx.test") {
		t.Error("a store error reported the host TLS-broken")
	}
	var nilP *Profiles
	if nilP.TLSBroken(t.Context(), "mx.test") || p.TLSBroken(t.Context(), "") {
		t.Error("nil or empty reported TLS-broken")
	}
	nilP.MarkTLSBroken(t.Context(), "mx.test")
	if TLSKey("a.b") != "mx:a.b:tls_broken" {
		t.Errorf("TLSKey = %q", TLSKey("a.b"))
	}
}
