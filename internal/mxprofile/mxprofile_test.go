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
