-- Per-MX token bucket. Refill and take must be one round trip: with two,
-- concurrent workers read the same token count and both spend it, which is how
-- a "1 request per 3 seconds" limit quietly becomes N per 3 seconds.
--
-- KEYS[1] = rt:mx:<domain>:bucket   hash {tokens, ts}
-- ARGV[1] = rate (tokens per second, float)
-- ARGV[2] = burst (max tokens, float)
-- ARGV[3] = now (unix seconds, float; pass the caller's clock, not Redis TIME,
--                so tests can control it)
-- ARGV[4] = requested tokens (usually 1)
--
-- Returns {allowed, tokens_left, retry_after_seconds}

local rate    = tonumber(ARGV[1])
local burst   = tonumber(ARGV[2])
local now     = tonumber(ARGV[3])
local want    = tonumber(ARGV[4])

local state   = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens  = tonumber(state[1])
local ts      = tonumber(state[2])

if tokens == nil then
  tokens = burst
  ts = now
end

local elapsed = math.max(0, now - ts)
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
local retry = 0
if tokens >= want then
  allowed = 1
  tokens = tokens - want
else
  retry = (want - tokens) / rate
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
-- Expire idle buckets: a bulk job touching 400 unique domains should not leave
-- 400 keys behind forever.
redis.call('EXPIRE', KEYS[1], 3600)

return {allowed, tostring(tokens), tostring(retry)}
