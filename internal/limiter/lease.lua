-- Per-pace-key session leases (plan 028). Acquire must be one round trip, for the
-- reason the bucket's take is: with two, concurrent workers both read "0 held"
-- and both open a session.
--
-- KEYS[1] = rt:mx:<pace_key>:inflight   sorted set, member = lease id, score = expiry (ms)
-- ARGV[1] = limit (max concurrent leases, integer >= 1)
-- ARGV[2] = now (unix milliseconds; the caller's clock, like the bucket)
-- ARGV[3] = ttl (milliseconds a lease lives unless released)
-- ARGV[4] = lease id
--
-- Returns {granted (1/0), held_after, retry_after_ms}
-- A lease from a node that died is dropped once its expiry passes, so a crash
-- can delay a family by at most one TTL and never wedge it.

local limit = tonumber(ARGV[1])
local now   = tonumber(ARGV[2])
local ttl   = tonumber(ARGV[3])
local id    = ARGV[4]

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local held = redis.call('ZCARD', KEYS[1])

if held < limit then
  redis.call('ZADD', KEYS[1], now + ttl, id)
  redis.call('PEXPIRE', KEYS[1], ttl)
  return {1, held + 1, 0}
end

local first = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
local retry = 0
if first[2] then
  retry = math.max(0, tonumber(first[2]) - now)
end
return {0, held, retry}
