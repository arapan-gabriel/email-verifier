package relay

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is enough Redis for this queue: strings, a list and a sorted set.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
	lists  map[string][]string
	zset   map[string]map[string]int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		values: map[string]string{},
		lists:  map[string][]string{},
		zset:   map[string]map[string]int64{},
	}
}

func (f *fakeStore) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeStore) Do(_ context.Context, args ...string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.ToUpper(args[0]) {
	case "SET":
		f.values[args[1]] = args[2]
		return "OK", nil
	case "DEL":
		delete(f.values, args[1])
		return int64(1), nil
	case "LPUSH":
		f.lists[args[1]] = append([]string{args[2]}, f.lists[args[1]]...)
		return int64(len(f.lists[args[1]])), nil
	case "RPOP":
		list := f.lists[args[1]]
		if len(list) == 0 {
			return nil, nil
		}
		last := list[len(list)-1]
		f.lists[args[1]] = list[:len(list)-1]
		return last, nil
	case "LLEN":
		return int64(len(f.lists[args[1]])), nil
	case "ZADD":
		score, _ := strconv.ParseInt(args[2], 10, 64)
		if f.zset[args[1]] == nil {
			f.zset[args[1]] = map[string]int64{}
		}
		f.zset[args[1]][args[3]] = score
		return int64(1), nil
	case "ZREM":
		if _, ok := f.zset[args[1]][args[2]]; !ok {
			return int64(0), nil
		}
		delete(f.zset[args[1]], args[2])
		return int64(1), nil
	case "ZCARD":
		return int64(len(f.zset[args[1]])), nil
	case "ZRANGEBYSCORE":
		cutoff, _ := strconv.ParseInt(args[3], 10, 64)
		var out []any
		for member, score := range f.zset[args[1]] {
			if score <= cutoff {
				out = append(out, member)
			}
		}
		return out, nil
	}
	return nil, nil
}

func testItem(id string) Item {
	return Item{ID: id, MailFrom: "bounces+x@test", RcptTo: "someone@example.com",
		Data: []byte("Subject: hi\r\n\r\nbody\r\n")}
}

func TestEnqueueThenNextReturnsTheMessage(t *testing.T) {
	q := NewQueue(newFakeStore(), 5)
	if err := q.Enqueue(t.Context(), testItem("m1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, ok, err := q.Next(t.Context())
	if err != nil || !ok {
		t.Fatalf("Next = %v, %v, %v", got, ok, err)
	}
	if got.ID != "m1" || got.RcptTo != "someone@example.com" || string(got.Data) == "" {
		t.Fatalf("the message came back changed: %+v", got)
	}
	if _, ok, _ := q.Next(t.Context()); ok {
		t.Error("the same message was handed out twice")
	}
}

// An id in the ready list pointing at nothing is the shape of a partial write.
// It must not stall the queue or resurrect an empty message.
func TestAnIdWithoutABodyIsDropped(t *testing.T) {
	store := newFakeStore()
	q := NewQueue(store, 5)
	_, _ = store.Do(t.Context(), "LPUSH", readyKey, "ghost")
	if _, ok, err := q.Next(t.Context()); ok || err != nil {
		t.Fatalf("Next = %v, %v — want nothing and no error", ok, err)
	}
}

// A retry is not due until its time arrives, and then it is due exactly once.
func TestRetryWaitsForItsTimeAndIsPromotedOnce(t *testing.T) {
	store := newFakeStore()
	q := NewQueue(store, 5)
	now := time.Unix(1_000_000, 0)
	q.Now = func() time.Time { return now }

	if err := q.Retry(t.Context(), testItem("m1"), time.Minute, "451 try later"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if _, ok, _ := q.Next(t.Context()); ok {
		t.Fatal("a retry came back before its time")
	}

	now = now.Add(61 * time.Second)
	got, ok, err := q.Next(t.Context())
	if err != nil || !ok {
		t.Fatalf("Next after the delay = %v, %v", ok, err)
	}
	if got.Attempts != 1 || got.LastErr == "" {
		t.Errorf("the attempt count and reason were lost: %+v", got)
	}
	if _, ok, _ := q.Next(t.Context()); ok {
		t.Error("promoted twice — the message would be sent twice")
	}
}

// Backoff has to grow, or a server that said "later" is asked again at the same
// rate, which is what gets a sender throttled harder rather than less.
func TestBackoffGrowsWithEachAttempt(t *testing.T) {
	store := newFakeStore()
	q := NewQueue(store, 9)
	now := time.Unix(1_000_000, 0)
	q.Now = func() time.Time { return now }

	item := testItem("m1")
	var delays []int64
	for attempt := 0; attempt < 3; attempt++ {
		if err := q.Retry(t.Context(), item, time.Minute, "451"); err != nil {
			t.Fatalf("Retry: %v", err)
		}
		delays = append(delays, store.zset[laterKey]["m1"]-now.Unix())
		item.Attempts++
	}
	if delays[0] >= delays[1] || delays[1] >= delays[2] {
		t.Fatalf("delays did not grow: %v", delays)
	}
}

// A message nobody will accept must stop being offered — and must stay
// findable, or "we sent it" and "we gave up" look identical afterwards.
func TestAMessageIsBuriedRatherThanRetriedForever(t *testing.T) {
	store := newFakeStore()
	q := NewQueue(store, 3)
	q.Now = func() time.Time { return time.Unix(1_000_000, 0) }

	item := testItem("m1")
	for item.Attempts < 3 {
		if err := q.Retry(t.Context(), item, time.Second, "451 later"); err != nil {
			t.Fatalf("Retry: %v", err)
		}
		item.Attempts++
	}
	if _, _, dead := q.Depth(t.Context()); dead != 1 {
		t.Fatalf("dead = %d, want 1", dead)
	}
	if _, ok := store.values[itemPrefix+"m1"]; !ok {
		t.Error("a buried message was deleted — nothing left to explain it")
	}
}

// Accepted means written before the caller is answered: a restart between the
// two would lose a message the caller believes was taken.
func TestAQueuedMessageSurvivesTheProcess(t *testing.T) {
	store := newFakeStore()
	if err := NewQueue(store, 5).Enqueue(t.Context(), testItem("m1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// A second Queue over the same store is what a restart looks like.
	got, ok, err := NewQueue(store, 5).Next(t.Context())
	if err != nil || !ok || got.ID != "m1" {
		t.Fatalf("after a restart: %v, %v, %v", got.ID, ok, err)
	}
}
