// Package mxprofile remembers facts about a recipient *server*, as opposed to
// facts about a recipient domain.
//
// The distinction is the whole point. A catch-all is a property of a domain and
// belongs to Data Scout, which already tracks it. A randomiser — a host that
// answers inconsistently, Microsoft being the archetype — is a property of the
// server, so it condemns every domain hosted there, including ones nobody has
// asked about yet. Getting that backwards reports a nonexistent mailbox as
// valid on the strength of a 250 that meant nothing.
package mxprofile

import (
	"context"
	"time"
)

// Store is the Redis subset this package needs.
type Store interface {
	Do(ctx context.Context, args ...string) (any, error)
	Get(ctx context.Context, key string) (string, bool, error)
}

// Key returns the randomiser key for an MX host.
func Key(mxHost string) string { return "mx:" + mxHost + ":randomiser" }

// Profiles records per-server verdicts.
type Profiles struct {
	store Store
	ttl   time.Duration
}

// New returns a Profiles. A zero ttl means one day: long enough that a bulk run
// pays for the discovery once, short enough that a server which stops
// randomising is re-examined.
func New(store Store, ttl time.Duration) *Profiles {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Profiles{store: store, ttl: ttl}
}

// IsRandomiser reports whether this host is known to answer inconsistently.
//
// A store failure returns false rather than an error. Unlike the rate budget,
// not knowing this costs accuracy, not safety: the caller simply probes again.
// Failing the request would trade a real answer for no answer.
func (p *Profiles) IsRandomiser(ctx context.Context, mxHost string) bool {
	if p == nil || mxHost == "" {
		return false
	}
	v, ok, err := p.store.Get(ctx, Key(mxHost))
	return err == nil && ok && v == "1"
}

// MarkRandomiser records the verdict for this host. Best effort, for the same
// reason.
func (p *Profiles) MarkRandomiser(ctx context.Context, mxHost string) {
	if p == nil || mxHost == "" {
		return
	}
	seconds := int64(p.ttl / time.Second)
	_, _ = p.store.Do(ctx, "SET", Key(mxHost), "1", "EX", itoa(seconds))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
