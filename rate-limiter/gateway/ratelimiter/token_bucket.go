package ratelimiter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenBucket implements a token bucket rate limiter using Redis
type TokenBucket struct {
	client     redis.Cmdable
	bucketSize int64
	refillRate float64 // tokens per second
}

// Result contains the rate limiting decision and metadata
type Result struct {
	Allowed   bool
	Remaining int64
	Limit     int64
	RetryAfter time.Duration
}

// Lua script for atomic token bucket operations
// This prevents race conditions by doing read-modify-write atomically
var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local bucket_size = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

-- Get current state
local tokens = tonumber(redis.call('HGET', key, 'tokens'))
local last_refill = tonumber(redis.call('HGET', key, 'last_refill'))

-- Initialize if first request
if tokens == nil then
    tokens = bucket_size
    last_refill = now
end

-- Calculate tokens to add based on time elapsed
local elapsed = now - last_refill
local tokens_to_add = elapsed * refill_rate
tokens = math.min(bucket_size, tokens + tokens_to_add)

-- Try to consume a token
local allowed = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
end

-- Calculate retry after (time until 1 token is available)
local retry_after = 0
if allowed == 0 then
    retry_after = math.ceil((1 - tokens) / refill_rate)
end

-- Save state
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('EXPIRE', key, 3600) -- Expire after 1 hour of inactivity

return {allowed, math.floor(tokens), retry_after}
`)

// NewTokenBucket creates a new token bucket rate limiter
// client can be either *redis.Client (standalone) or *redis.ClusterClient (cluster mode)
func NewTokenBucket(client redis.Cmdable, bucketSize int64, refillRate float64) *TokenBucket {
	return &TokenBucket{
		client:     client,
		bucketSize: bucketSize,
		refillRate: refillRate,
	}
}

// Allow checks if a request should be allowed for the given key
func (tb *TokenBucket) Allow(ctx context.Context, key string) (*Result, error) {
	now := float64(time.Now().UnixNano()) / float64(time.Second)

	result, err := tokenBucketScript.Run(ctx, tb.client, []string{key},
		tb.bucketSize,
		tb.refillRate,
		now,
	).Int64Slice()

	if err != nil {
		return nil, err
	}

	return &Result{
		Allowed:    result[0] == 1,
		Remaining:  result[1],
		Limit:      tb.bucketSize,
		RetryAfter: time.Duration(result[2]) * time.Second,
	}, nil
}

// IsHealthy checks if Redis connection is working
func (tb *TokenBucket) IsHealthy(ctx context.Context) bool {
	return tb.client.Ping(ctx).Err() == nil
}
