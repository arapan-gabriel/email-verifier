package suppress

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu      sync.Mutex
	set     map[string]bool
	kv      map[string]string
	err     error
	written []string // every argument this store was ever handed
}

func newStore() *fakeStore {
	return &fakeStore{set: map[string]bool{}, kv: map[string]string{}}
}

func (f *fakeStore) Do(_ context.Context, args ...string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, args...)
	if f.err != nil {
		return nil, f.err
	}
	switch args[0] {
	case "SISMEMBER":
		if f.set[args[2]] {
			return int64(1), nil
		}
		return int64(0), nil
	case "SADD":
		for _, d := range args[2:] {
			f.set[d] = true
		}
		return int64(len(args) - 2), nil
	case "DEL":
		f.set = map[string]bool{}
		return int64(1), nil
	case "SCARD":
		return int64(len(f.set)), nil
	}
	return nil, nil
}

func (f *fakeStore) Get(_ context.Context, k string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[k]
	return v, ok, nil
}

func (f *fakeStore) Set(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, k, v)
	f.kv[k] = v
	return nil
}

func (f *fakeStore) everythingWritten() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.written, " ")
}

const salt = "deployment-salt"

func newList(t *testing.T, store Store, addrs ...string) *List {
	t.Helper()
	l := New(Options{Salt: salt, Store: store})
	digests := make([]string, 0, len(addrs))
	for _, a := range addrs {
		digests = append(digests, Hash(salt, a))
	}
	if err := l.Import(t.Context(), "v1", digests, true); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return l
}

func TestSuppressedByAddress(t *testing.T) {
	l := newList(t, newStore(), "forgotten@example.test")
	for _, in := range []string{
		"forgotten@example.test",
		"Forgotten@Example.Test", // case is normalised on both sides
		"  forgotten@example.test  ",
	} {
		hit, reason, err := l.Suppressed(t.Context(), in)
		if err != nil {
			t.Fatalf("Suppressed(%q): %v", in, err)
		}
		if !hit {
			t.Errorf("Suppressed(%q) = false", in)
		}
		if !strings.Contains(reason, "address") {
			t.Errorf("reason = %q", reason)
		}
	}
	if hit, _, _ := l.Suppressed(t.Context(), "someone.else@example.test"); hit {
		t.Error("an unrelated address at the same domain was suppressed")
	}
}

// The source model suppresses a whole domain too (domain_host with no
// full_name), and that has to cover every address on it.
func TestSuppressedByDomain(t *testing.T) {
	l := newList(t, newStore(), "gone.test")
	for _, in := range []string{"anyone@gone.test", "someone.else@GONE.test"} {
		hit, reason, err := l.Suppressed(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !hit {
			t.Errorf("Suppressed(%q) = false; the whole domain is suppressed", in)
		}
		if !strings.Contains(reason, "domain") {
			t.Errorf("reason = %q, want it to name the domain", reason)
		}
	}
	if hit, _, _ := l.Suppressed(t.Context(), "anyone@still-here.test"); hit {
		t.Error("a different domain was suppressed")
	}
}

// The point of the whole design: what lands in Redis must not be readable back
// into a mailing list.
func TestNoAddressIsEverWritten(t *testing.T) {
	store := newStore()
	l := newList(t, store, "forgotten@example.test", "gone.test")
	if _, _, err := l.Suppressed(t.Context(), "forgotten@example.test"); err != nil {
		t.Fatal(err)
	}
	written := store.everythingWritten()
	for _, leak := range []string{"forgotten", "example.test", "gone.test", "@"} {
		if strings.Contains(written, leak) {
			t.Errorf("%q reached the store:\n%s", leak, written)
		}
	}
}

// An address sent instead of a digest is refused rather than stored — that
// would be exactly the leak the hashing exists to prevent.
func TestImportRefusesAnythingThatIsNotADigest(t *testing.T) {
	l := New(Options{Salt: salt, Store: newStore()})
	for _, bad := range []string{"forgotten@example.test", "short", strings.Repeat("z", 64)} {
		if err := l.Import(t.Context(), "v1", []string{bad}, true); err == nil {
			t.Errorf("Import accepted %q", bad)
		}
	}
}

// Replace is what makes a removal at the source propagate.
func TestReplaceClearsAndAddDoesNot(t *testing.T) {
	store := newStore()
	l := newList(t, store, "first@example.test")

	if err := l.Import(t.Context(), "v2", []string{Hash(salt, "second@example.test")}, false); err != nil {
		t.Fatal(err)
	}
	if hit, _, _ := l.Suppressed(t.Context(), "first@example.test"); !hit {
		t.Error("add dropped an earlier entry")
	}

	if err := l.Import(t.Context(), "v3", []string{Hash(salt, "third@example.test")}, true); err != nil {
		t.Fatal(err)
	}
	if hit, _, _ := l.Suppressed(t.Context(), "first@example.test"); hit {
		t.Error("replace kept an entry the source no longer has")
	}
	if hit, _, _ := l.Suppressed(t.Context(), "third@example.test"); !hit {
		t.Error("replace did not apply the new export")
	}
}

// A different salt means every lookup misses — silently. That is why an empty
// salt refuses to boot, and why this is worth pinning.
func TestSaltIsPartOfTheDigest(t *testing.T) {
	if Hash("a", "x@y.test") == Hash("b", "x@y.test") {
		t.Fatal("the salt does not affect the digest")
	}
	if got := len(Hash(salt, "x@y.test")); got != 64 {
		t.Errorf("digest length = %d, want 64 hex characters", got)
	}
}

// On the verify path this is a redundancy; a Redis blip must not become "we
// cannot answer anything".
func TestStoreFailureIsReportedNotSwallowed(t *testing.T) {
	store := newStore()
	store.err = errors.New("connection refused")
	l := New(Options{Salt: salt, Store: store})
	hit, _, err := l.Suppressed(t.Context(), "x@y.test")
	if err == nil {
		t.Fatal("a store failure was swallowed; the caller cannot decide what it means")
	}
	if hit {
		t.Error("a store failure was reported as a suppression")
	}
}

func TestStatusReportsStaleness(t *testing.T) {
	store := newStore()
	l := New(Options{Salt: salt, Store: store, Stale: time.Hour})

	if st := l.Status(t.Context()); !st.Stale {
		t.Error("a list that was never imported is not stale")
	}
	if err := l.Import(t.Context(), "v1", []string{Hash(salt, "a@b.test")}, true); err != nil {
		t.Fatal(err)
	}
	st := l.Status(t.Context())
	if st.Stale {
		t.Error("a fresh import is stale")
	}
	if st.Version != "v1" || st.Size != 1 || !st.Enabled {
		t.Errorf("Status = %+v", st)
	}
}

func TestDisabledWithoutSaltOrStore(t *testing.T) {
	for name, l := range map[string]*List{
		"no salt":  New(Options{Store: newStore()}),
		"no store": New(Options{Salt: salt}),
		"nil":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			if l.Enabled() {
				t.Error("reported enabled")
			}
			hit, _, err := l.Suppressed(context.Background(), "x@y.test")
			if hit || err != nil {
				t.Errorf("Suppressed = %v, %v; a disabled list must refuse nothing", hit, err)
			}
		})
	}
}
