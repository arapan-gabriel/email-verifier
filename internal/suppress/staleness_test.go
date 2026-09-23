package suppress

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Stale is what the relay asks before every send (plan 027): fresh after an
// import, stale once the window passes, and stale for a list never imported.
func TestStaleFollowsTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New(Options{Salt: salt, Store: newStore(), Stale: time.Hour})
		if !l.Stale(t.Context()) {
			t.Error("a list never imported must be stale")
		}
		if err := l.Import(t.Context(), "v1", []string{Hash(salt, "a@b.test")}, true); err != nil {
			t.Fatal(err)
		}
		if l.Stale(t.Context()) {
			t.Error("a fresh import is stale")
		}
		time.Sleep(61 * time.Minute)
		if !l.Stale(t.Context()) {
			t.Error("an import older than the window is not stale")
		}
	})
}

// A batch larger than one SADD chunk is imported whole.
func TestImportChunksLargeExports(t *testing.T) {
	store := newStore()
	l := New(Options{Salt: salt, Store: store})
	digests := make([]string, 1203)
	for i := range digests {
		digests[i] = Hash(salt, strings.Repeat("x", i+1)+"@b.test")
	}
	if err := l.Import(t.Context(), "v1", digests, true); err != nil {
		t.Fatal(err)
	}
	if st := l.Status(t.Context()); st.Size != 1203 {
		t.Errorf("size = %d, want 1203", st.Size)
	}
}

type wrongTypeStore struct{ *fakeStore }

func (w wrongTypeStore) Do(ctx context.Context, args ...string) (any, error) {
	if args[0] == "SISMEMBER" {
		return "1", nil
	}
	return w.fakeStore.Do(ctx, args...)
}

// An unexpected reply type is an error for the caller to judge, never a hit
// or a miss.
func TestMemberRejectsAnUnexpectedReply(t *testing.T) {
	l := New(Options{Salt: salt, Store: wrongTypeStore{newStore()}})
	if _, _, err := l.Suppressed(t.Context(), "a@b.test"); err == nil {
		t.Error("a non-integer SISMEMBER reply was read as an answer")
	}
}

// A store that fails mid-import says so.
func TestImportSurfacesStoreFailures(t *testing.T) {
	store := newStore()
	store.err = errors.New("READONLY")
	l := New(Options{Salt: salt, Store: store})
	if err := l.Import(t.Context(), "v1", []string{Hash(salt, "a@b.test")}, true); err == nil {
		t.Error("a failed DEL was swallowed")
	}
	if err := l.Import(t.Context(), "v1", []string{Hash(salt, "a@b.test")}, false); err == nil {
		t.Error("a failed SADD was swallowed")
	}
	if err := New(Options{}).Import(t.Context(), "v1", nil, true); err == nil {
		t.Error("an unconfigured list accepted an import")
	}
}
