package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Store is the Redis surface this queue needs, declared here in the package
// that calls it (ENGINEERING-STANDARDS §2).
type Store interface {
	Do(ctx context.Context, args ...string) (any, error)
	Get(ctx context.Context, key string) (string, bool, error)
}

const (
	readyKey   = "relay:ready" // list: ids waiting to go out now
	laterKey   = "relay:later" // sorted set: id → unix seconds it is due
	itemPrefix = "relay:msg:"  // hash-free: one JSON blob per message
	deadKey    = "relay:dead"  // list: gave up, kept for a person to look at
)

// Item is one queued message, as stored.
type Item struct {
	ID       string    `json:"id"`
	MailFrom string    `json:"mail_from"`
	RcptTo   string    `json:"rcpt_to"`
	Data     []byte    `json:"data"`
	Attempts int       `json:"attempts"`
	Queued   time.Time `json:"queued_at"`
	// LastErr is what the previous attempt was told, kept so a message that
	// dies after five tries can say why without a log search.
	LastErr string `json:"last_err,omitempty"`
}

// Queue is a durable outbound queue on Redis.
//
// Durable matters more here than anywhere else in this service: a verification
// that is lost can be asked again, and a message accepted from a caller and
// then dropped cannot. Redis carries AOF with `appendfsync everysec` on this
// host for exactly this class of state (plan 006 chose it, plan 013 deployed
// it) — so "accepted" means written there, before the caller is answered.
type Queue struct {
	store Store
	// MaxAttempts bounds retrying. A message that a server has refused five
	// times is not going to be accepted on the sixth, and continuing to offer
	// it is how a sender's reputation is spent on one bad address.
	MaxAttempts int
	Now         func() time.Time
}

// NewQueue returns a Queue over store. A non-positive maxAttempts takes the
// default of five.
func NewQueue(store Store, maxAttempts int) *Queue {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &Queue{store: store, MaxAttempts: maxAttempts, Now: time.Now}
}

// Enqueue stores the message and makes it ready. The order is deliberate: the
// body is written first, so an id can never reach the ready list pointing at
// nothing.
func (q *Queue) Enqueue(ctx context.Context, item Item) error {
	if item.ID == "" {
		return errors.New("relay: an item needs an id")
	}
	blob, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("relay: encoding item: %w", err)
	}
	if _, err := q.store.Do(ctx, "SET", itemPrefix+item.ID, string(blob)); err != nil {
		return fmt.Errorf("relay: storing item: %w", err)
	}
	if _, err := q.store.Do(ctx, "LPUSH", readyKey, item.ID); err != nil {
		return fmt.Errorf("relay: queueing item: %w", err)
	}
	return nil
}

// Next takes the oldest ready message, after promoting anything whose retry is
// due. Returns false when there is nothing to do.
func (q *Queue) Next(ctx context.Context) (Item, bool, error) {
	if err := q.promote(ctx); err != nil {
		return Item{}, false, err
	}
	raw, err := q.store.Do(ctx, "RPOP", readyKey)
	if err != nil {
		return Item{}, false, fmt.Errorf("relay: reading queue: %w", err)
	}
	id, ok := asString(raw)
	if !ok || id == "" {
		return Item{}, false, nil
	}
	blob, found, err := q.store.Get(ctx, itemPrefix+id)
	if err != nil {
		return Item{}, false, fmt.Errorf("relay: reading item: %w", err)
	}
	if !found {
		// The id outlived its body — a partial write, or someone cleaning up
		// by hand. Dropping the id is right: there is nothing left to send.
		return Item{}, false, nil
	}
	var item Item
	if err := json.Unmarshal([]byte(blob), &item); err != nil {
		return Item{}, false, fmt.Errorf("relay: decoding item %s: %w", id, err)
	}
	return item, true, nil
}

// Retry puts a message back with a delay, or buries it once it has had enough
// attempts.
//
// Backoff is exponential from the caller's base. A server that said "later"
// means it, and asking again immediately is both useless and the behaviour that
// gets a sender throttled harder.
func (q *Queue) Retry(ctx context.Context, item Item, base time.Duration, reason string) error {
	item.Attempts++
	item.LastErr = reason
	if item.Attempts >= q.MaxAttempts {
		return q.bury(ctx, item)
	}
	blob, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("relay: encoding item: %w", err)
	}
	if _, err := q.store.Do(ctx, "SET", itemPrefix+item.ID, string(blob)); err != nil {
		return fmt.Errorf("relay: storing item: %w", err)
	}
	delay := base << (item.Attempts - 1)
	due := q.Now().Add(delay).Unix()
	if _, err := q.store.Do(ctx, "ZADD", laterKey, strconv.FormatInt(due, 10), item.ID); err != nil {
		return fmt.Errorf("relay: scheduling retry: %w", err)
	}
	return nil
}

// Done removes a delivered message. Nothing about it is kept: this service
// holds no business data (ARCHITECTURE), and the caller owns the record of what
// it asked to be sent.
func (q *Queue) Done(ctx context.Context, id string) error {
	if _, err := q.store.Do(ctx, "DEL", itemPrefix+id); err != nil {
		return fmt.Errorf("relay: removing item: %w", err)
	}
	return nil
}

// bury moves a message out of circulation, keeping it addressable so a person
// can see what happened. Deleting it silently would make "we sent it" and "we
// gave up" indistinguishable after the fact.
func (q *Queue) bury(ctx context.Context, item Item) error {
	blob, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("relay: encoding item: %w", err)
	}
	if _, err := q.store.Do(ctx, "SET", itemPrefix+item.ID, string(blob)); err != nil {
		return fmt.Errorf("relay: storing item: %w", err)
	}
	if _, err := q.store.Do(ctx, "LPUSH", deadKey, item.ID); err != nil {
		return fmt.Errorf("relay: burying item: %w", err)
	}
	return nil
}

// promote moves everything whose retry time has arrived back onto the ready
// list. Read-then-remove rather than an atomic pop: a message promoted twice is
// sent twice, so removal happens before it is made ready.
func (q *Queue) promote(ctx context.Context) error {
	now := strconv.FormatInt(q.Now().Unix(), 10)
	raw, err := q.store.Do(ctx, "ZRANGEBYSCORE", laterKey, "-inf", now)
	if err != nil {
		return fmt.Errorf("relay: reading the retry set: %w", err)
	}
	for _, id := range asStrings(raw) {
		removed, err := q.store.Do(ctx, "ZREM", laterKey, id)
		if err != nil {
			return fmt.Errorf("relay: promoting %s: %w", id, err)
		}
		// ZREM reports how many it removed. Zero means another drainer got
		// there first, and pushing anyway would send the message twice.
		if n, ok := asInt(removed); !ok || n == 0 {
			continue
		}
		if _, err := q.store.Do(ctx, "LPUSH", readyKey, id); err != nil {
			return fmt.Errorf("relay: requeueing %s: %w", id, err)
		}
	}
	return nil
}

// Depth reports what is waiting, for the metrics and for a person asking
// whether anything is stuck.
func (q *Queue) Depth(ctx context.Context) (ready, later, dead int) {
	if n, err := q.store.Do(ctx, "LLEN", readyKey); err == nil {
		if v, ok := asInt(n); ok {
			ready = v
		}
	}
	if n, err := q.store.Do(ctx, "ZCARD", laterKey); err == nil {
		if v, ok := asInt(n); ok {
			later = v
		}
	}
	if n, err := q.store.Do(ctx, "LLEN", deadKey); err == nil {
		if v, ok := asInt(n); ok {
			dead = v
		}
	}
	return ready, later, dead
}

func asString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	}
	return "", false
}

func asStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := asString(item); ok {
			out = append(out, s)
		}
	}
	return out
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int64:
		return int(t), true
	case int:
		return t, true
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n, true
		}
	}
	return 0, false
}
