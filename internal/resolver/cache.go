package resolver

import (
	"net/netip"
	"sync"
	"time"
)

// cache remembers vetted answers and refusals for a short while.
//
// Its purpose is not speed. The deployed node runs a caching resolver already,
// and a Redis round trip to avoid a lookup the OS has cached would make the hot
// path slower. Its purpose is that this service must not fall over when
// deployed somewhere resolv.conf points straight at a public resolver, where a
// 500-domain bulk job at one provider means 500 identical queries.
//
// It stores only **vetted** results. Caching before the guard would be a way to
// smuggle a refused address back in.
type cache struct {
	mu      sync.Mutex
	entries map[string]entry
	ttl     time.Duration
	negTTL  time.Duration
	max     int
}

type entry struct {
	addrs   []netip.Addr
	err     error // a *BlockedError or a lookup failure, replayed as-is
	expires time.Time
}

func newCache(ttl, negTTL time.Duration, size int) *cache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if negTTL <= 0 {
		negTTL = time.Minute
	}
	if size <= 0 {
		size = 4096
	}
	return &cache{entries: make(map[string]entry), ttl: ttl, negTTL: negTTL, max: size}
}

func (c *cache) get(host string) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[host]
	if !ok {
		return entry{}, false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, host)
		return entry{}, false
	}
	return e, true
}

func (c *cache) put(host string, addrs []netip.Addr, err error) {
	ttl := c.ttl
	if err != nil {
		// A refusal or a failure is held briefly: a domain pointing its MX
		// inward should cost one lookup rather than one per request, but a
		// misconfiguration that gets fixed should not be remembered for long.
		ttl = c.negTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[host] = entry{addrs: addrs, err: err, expires: time.Now().Add(ttl)}
}

// evictLocked drops expired entries, and if that frees nothing, drops an
// arbitrary one. A perfect LRU is not worth the bookkeeping for a table whose
// whole job is to survive a bulk job at one provider.
func (c *cache) evictLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

// len reports the number of entries held, for tests.
func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
