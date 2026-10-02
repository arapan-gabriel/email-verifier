package limiter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/redis"
)

// scriptedStore answers every Run with a fixed reply or error.
type scriptedStore struct {
	reply any
	err   error
	keys  []string
	args  []string
}

func (s *scriptedStore) Run(_ context.Context, _ *redis.Script, keys, args []string) (any, error) {
	s.keys, s.args = keys, args
	return s.reply, s.err
}

func TestLeaseParsesAGrant(t *testing.T) {
	s := &scriptedStore{reply: []any{int64(1), int64(1), int64(0)}}
	d, err := NewLeases(s).Lease(t.Context(), "@microsoft-eop", 1, 50*time.Second)
	if err != nil || !d.Granted || d.ID == "" || d.Held != 1 {
		t.Fatalf("Lease = %+v, %v", d, err)
	}
	if s.keys[0] != "rt:mx:@microsoft-eop:inflight" {
		t.Errorf("key = %q", s.keys[0])
	}
	if s.args[0] != "1" || s.args[2] != "50000" {
		t.Errorf("args = %v, want limit 1 and ttl 50000 ms", s.args)
	}
}

func TestLeaseParsesARefusalWithItsWait(t *testing.T) {
	s := &scriptedStore{reply: []any{int64(0), int64(1), int64(1500)}}
	d, err := NewLeases(s).Lease(t.Context(), "k", 1, time.Second)
	if err != nil || d.Granted || d.ID != "" || d.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("Lease = %+v, %v", d, err)
	}
}

// Invariant 5: no answer from the lease set is no session.
func TestLeaseFailsClosed(t *testing.T) {
	down := errors.New("connection refused")
	if _, err := NewLeases(&scriptedStore{err: down}).Lease(t.Context(), "k", 1, time.Second); !errors.Is(err, down) {
		t.Fatalf("err = %v, want the store's", err)
	}
	for _, bad := range []any{"OK", []any{int64(1)}, []any{"x", int64(1), int64(0)}} {
		if _, err := NewLeases(&scriptedStore{reply: bad}).Lease(t.Context(), "k", 1, time.Second); err == nil {
			t.Errorf("reply %#v accepted", bad)
		}
	}
}

func TestLeaseRejectsNonsense(t *testing.T) {
	l := NewLeases(&scriptedStore{})
	for _, tc := range []struct {
		key   string
		limit int
		ttl   time.Duration
	}{{"", 1, time.Second}, {"k", 0, time.Second}, {"k", 1, 0}} {
		if _, err := l.Lease(t.Context(), tc.key, tc.limit, tc.ttl); err == nil {
			t.Errorf("Lease(%q, %d, %s) accepted", tc.key, tc.limit, tc.ttl)
		}
	}
}

func TestReleaseIsANoOpWithoutAnID(t *testing.T) {
	s := &scriptedStore{err: errors.New("must not be called")}
	if err := NewLeases(s).Release(t.Context(), "k", ""); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseReportsTheStoreError(t *testing.T) {
	s := &scriptedStore{err: errors.New("down")}
	err := NewLeases(s).Release(t.Context(), "k", "id")
	if err == nil || !strings.Contains(err.Error(), "release") {
		t.Fatalf("err = %v", err)
	}
}
