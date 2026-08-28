package limiter

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// fakeStore stands in for Redis and, more usefully, evaluates the same
// arithmetic the Lua script does, so the caller's contract is exercised without
// a server.
type fakeStore struct {
	tokens float64
	burst  float64
	err    error
	calls  int
	keys   []string
}

func (f *fakeStore) Run(_ context.Context, _ *redis.Script, keys, args []string) (any, error) {
	f.calls++
	f.keys = append(f.keys, keys...)
	if f.err != nil {
		return nil, f.err
	}
	want, _ := strconv.ParseFloat(args[3], 64)
	allowed := int64(0)
	retry := 0.0
	if f.tokens >= want {
		allowed, f.tokens = 1, f.tokens-want
	} else {
		rate, _ := strconv.ParseFloat(args[0], 64)
		retry = (want - f.tokens) / rate
	}
	return []any{allowed, strconv.FormatFloat(f.tokens, 'f', -1, 64), strconv.FormatFloat(retry, 'f', -1, 64)}, nil
}

func TestTakeAllowsThenRefuses(t *testing.T) {
	f := &fakeStore{tokens: 1, burst: 1}
	l := New(f)

	d, err := l.Take(t.Context(), "mx.example.test", 1, 1)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !d.Allowed {
		t.Fatal("first take refused with a full bucket")
	}

	d, err = l.Take(t.Context(), "mx.example.test", 1, 1)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if d.Allowed {
		t.Error("second take allowed from an empty bucket")
	}
	if d.RetryAfter <= 0 {
		t.Error("a refusal carries no retry hint")
	}
}

// Invariant 4: one bucket per recipient MX, shared by every node.
func TestTakeUsesTheContractKey(t *testing.T) {
	f := &fakeStore{tokens: 5}
	if _, err := New(f).Take(t.Context(), "gmail-smtp-in.l.google.com", 1, 1); err != nil {
		t.Fatal(err)
	}
	want := "rt:mx:gmail-smtp-in.l.google.com:bucket"
	if len(f.keys) != 1 || f.keys[0] != want {
		t.Errorf("keys = %v, want [%s]", f.keys, want)
	}
}

// Take and refill must be one round trip: with two, concurrent workers read the
// same token count and both spend it.
func TestTakeIsOneRoundTrip(t *testing.T) {
	f := &fakeStore{tokens: 10}
	if _, err := New(f).Take(t.Context(), "mx.example.test", 1, 1); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("made %d calls, want 1", f.calls)
	}
}

// The caller must be able to fail closed (invariant 5), so a store failure has
// to surface as an error rather than a permissive default.
func TestTakeSurfacesStoreFailure(t *testing.T) {
	boom := errors.New("connection refused")
	_, err := New(&fakeStore{err: boom}).Take(t.Context(), "mx.example.test", 1, 1)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the store failure wrapped", err)
	}
}

func TestTakeRejectsNonsense(t *testing.T) {
	l := New(&fakeStore{tokens: 1})
	if _, err := l.Take(t.Context(), "", 1, 1); err == nil {
		t.Error("empty mx host accepted")
	}
	if _, err := l.Take(t.Context(), "mx.test", 0, 1); err == nil {
		t.Error("zero rate accepted")
	}
	if _, err := l.Take(t.Context(), "mx.test", 1, 0); err == nil {
		t.Error("zero burst accepted")
	}
}

func TestParseDecisionRejectsGarbage(t *testing.T) {
	for name, raw := range map[string]any{
		"not an array": "nope",
		"wrong length": []any{int64(1)},
		"bad allowed":  []any{"1", "1", "0"},
		"bad tokens":   []any{int64(1), "abc", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDecision(raw); err == nil {
				t.Error("parseDecision accepted a malformed reply")
			}
		})
	}
}

// The script is embedded so a missing file cannot make the limiter fail open in
// the worst possible place.
func TestScriptIsEmbedded(t *testing.T) {
	if !strings.Contains(tokenBucket, "HMGET") || !strings.Contains(tokenBucket, "EXPIRE") {
		t.Fatalf("embedded script does not look like the token bucket:\n%s", tokenBucket)
	}
}
