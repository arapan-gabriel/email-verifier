// Package suppress refuses to touch an address someone asked to be forgotten.
//
// It holds **no addresses**. A suppression list is a list of email addresses,
// and copying one onto a second host — for a mechanism whose entire purpose is
// erasure — would create the liability the mechanism exists to discharge. What
// is stored here is a salted digest: membership is checkable, the plaintext is
// not recoverable from it, and erasure is deleting one key.
//
// Data Scout owns the list and already checks it three times before calling
// (`privacy_service.is_email_suppressed`, from `verify`, from the prefetch leg,
// and again in the bulk task). This is a second line, and the fail policy
// follows from that: on the verify path a missing or stale copy logs loudly and
// does not fail the request, because failing would trade a real capability for
// a control that has already been applied. Phase C relay has no upstream check
// between the queue and the socket, and fails closed.
package suppress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Store is the Redis subset this package needs.
type Store interface {
	Do(ctx context.Context, args ...string) (any, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, val string) error
}

const (
	setKey     = "suppress:hashes"
	versionKey = "suppress:version"
	updatedKey = "suppress:updated_at"
)

// Options configures a List.
type Options struct {
	// Salt makes the digests specific to this deployment, so a stored hash
	// cannot be matched against a rainbow table of common addresses. Both sides
	// must use the same one.
	Salt string
	// Stale is how old an export may be before it is no longer trusted to be
	// complete. Exceeding it is loud, not fatal — see the package comment.
	Stale time.Duration
	Store Store
}

// List answers whether an address or its domain has been suppressed.
type List struct {
	opts Options
}

// New returns a List. A zero Stale means one day.
func New(opts Options) *List {
	if opts.Stale <= 0 {
		opts.Stale = 24 * time.Hour
	}
	return &List{opts: opts}
}

// Enabled reports whether checking will happen at all.
func (l *List) Enabled() bool {
	return l != nil && l.opts.Salt != "" && l.opts.Store != nil
}

// Hash is the digest both sides compute. Exported because Data Scout has to
// produce exactly this, and a mismatch is silent: every lookup simply misses.
//
//	sha256(salt + "\x00" + lowercased, trimmed value)
func Hash(salt, value string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + strings.ToLower(strings.TrimSpace(value))))
	return hex.EncodeToString(sum[:])
}

// Suppressed reports whether this address, or the whole of its domain, has been
// suppressed — and the reason to record if so.
//
// A store failure is not a refusal. On the verify path this is a redundancy,
// and turning a Redis blip into "we cannot answer anything" would be worse than
// the thing it guards against.
func (l *List) Suppressed(ctx context.Context, email string) (bool, string, error) {
	if !l.Enabled() || email == "" {
		return false, "", nil
	}
	addr := strings.ToLower(strings.TrimSpace(email))
	candidates := []struct{ kind, value string }{{"address", addr}}
	if at := strings.LastIndex(addr, "@"); at >= 0 && at+1 < len(addr) {
		candidates = append(candidates, struct{ kind, value string }{"domain", addr[at+1:]})
	}

	for _, c := range candidates {
		hit, err := l.member(ctx, Hash(l.opts.Salt, c.value))
		if err != nil {
			return false, "", err
		}
		if hit {
			return true, "suppressed by " + c.kind, nil
		}
	}
	return false, "", nil
}

func (l *List) member(ctx context.Context, digest string) (bool, error) {
	v, err := l.opts.Store.Do(ctx, "SISMEMBER", setKey, digest)
	if err != nil {
		return false, err
	}
	n, ok := v.(int64)
	if !ok {
		return false, fmt.Errorf("suppress: SISMEMBER returned %T", v)
	}
	return n == 1, nil
}

// Import applies an export. replace clears whatever a previous one left, which
// is what makes a removal from the source propagate; add only grows the set.
func (l *List) Import(ctx context.Context, version string, digests []string, replace bool) error {
	if !l.Enabled() {
		return errors.New("suppress: not configured (no salt or no store)")
	}
	for _, d := range digests {
		if !isDigest(d) {
			// An address here would be exactly the leak this package exists to
			// prevent, so a non-digest is refused rather than stored.
			return fmt.Errorf("suppress: entry is not a sha256 digest; addresses must never be sent")
		}
	}
	if replace {
		if _, err := l.opts.Store.Do(ctx, "DEL", setKey); err != nil {
			return fmt.Errorf("suppress: clearing the previous export: %w", err)
		}
	}
	// Batched, because a full export is one round trip per chunk rather than
	// one per address.
	const chunk = 500
	for i := 0; i < len(digests); i += chunk {
		end := min(i+chunk, len(digests))
		args := append([]string{"SADD", setKey}, digests[i:end]...)
		if _, err := l.opts.Store.Do(ctx, args...); err != nil {
			return fmt.Errorf("suppress: importing: %w", err)
		}
	}
	if err := l.opts.Store.Set(ctx, versionKey, version); err != nil {
		return err
	}
	return l.opts.Store.Set(ctx, updatedKey, strconv.FormatInt(time.Now().Unix(), 10))
}

// Status is what an operator and the staleness check both read.
type Status struct {
	Enabled bool      `json:"enabled"`
	Version string    `json:"version,omitempty"`
	Updated time.Time `json:"updated_at,omitzero"`
	Size    int64     `json:"size"`
	Stale   bool      `json:"stale"`
}

// Status reports the state of the local copy.
func (l *List) Status(ctx context.Context) Status {
	st := Status{Enabled: l.Enabled()}
	if !st.Enabled {
		return st
	}
	if v, ok, err := l.opts.Store.Get(ctx, versionKey); err == nil && ok {
		st.Version = v
	}
	if v, ok, err := l.opts.Store.Get(ctx, updatedKey); err == nil && ok {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			st.Updated = time.Unix(sec, 0)
		}
	}
	if v, err := l.opts.Store.Do(ctx, "SCARD", setKey); err == nil {
		if n, ok := v.(int64); ok {
			st.Size = n
		}
	}
	// Never imported, or older than the threshold: the copy cannot be trusted
	// to be complete. Loud, not fatal.
	st.Stale = st.Updated.IsZero() || time.Since(st.Updated) > l.opts.Stale
	return st
}

func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
