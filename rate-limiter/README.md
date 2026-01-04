# Building a Distributed Rate Limiter with Go and Redis

Rate limiting is one of those deceptively simple problems. On the surface, it's just counting requests. In practice, it's a distributed systems challenge involving race conditions, failure modes, and latency trade-offs.

This repository contains a production-ready rate limiter implementation using the Token Bucket algorithm, Go, and Redis. We'll walk through the key design decisions and the reasoning behind them.

## The Problem

You need to limit API requests to N per second per client. Sounds simple until you consider:

- **Distributed deployment**: Multiple gateway instances, single source of truth
- **Race conditions**: Two requests checking the counter simultaneously
- **Failure modes**: What happens when Redis goes down?
- **Latency budget**: Every millisecond counts at the edge

## Architecture

### Standalone Mode

```
┌──────────┐     ┌──────────────────┐     ┌─────────┐
│  Client  │────▶│  Gateway (:8080) │────▶│ Backend │
└──────────┘     └────────┬─────────┘     └─────────┘
                          │
                    ┌─────▼─────┐
                    │   Redis   │
                    │  (:6379)  │
                    └───────────┘
```

### Cluster Mode (High Availability)

```
┌──────────┐     ┌──────────────────┐     ┌─────────┐
│  Client  │────▶│  Gateway (:8080) │────▶│ Backend │
└──────────┘     └────────┬─────────┘     └─────────┘
                          │
         ┌────────────────┼────────────────┐
         │                │                │
   ┌─────▼─────┐    ┌─────▼─────┐    ┌─────▼─────┐
   │  Master 1 │    │  Master 2 │    │  Master 3 │
   │  (:7000)  │    │  (:7001)  │    │  (:7002)  │
   └─────┬─────┘    └─────┬─────┘    └─────┬─────┘
         │                │                │
   ┌─────▼─────┐    ┌─────▼─────┐    ┌─────▼─────┐
   │  Replica  │    │  Replica  │    │  Replica  │
   │  (:7003)  │    │  (:7004)  │    │  (:7005)  │
   └───────────┘    └───────────┘    └───────────┘
```

The gateway acts as a reverse proxy. Every request passes through the rate limiter middleware before reaching the backend. Redis stores the token bucket state, enabling horizontal scaling of gateway instances.

### Component Interaction

```
Request Flow:
1. Client → Gateway (HTTP request on :8080)
2. Gateway extracts client IP (X-Forwarded-For → X-Real-IP → RemoteAddr)
3. Gateway → Redis (Lua script execution, atomic token check)
4. If allowed: Gateway → Backend (:8081) → Response to client
5. If denied: Gateway returns 429 with retry-after header
```

**Key Design Decisions:**
- **Single Source of Truth**: Redis holds all rate limit state
- **Horizontal Scaling**: Multiple gateway instances share Redis state
- **Fail-Open Strategy**: Requests allowed when Redis unavailable (with warning header)
- **Atomic Operations**: Lua scripts prevent race conditions

## Why Token Bucket?

We evaluated four algorithms:

| Algorithm | Burst Handling | Memory | Accuracy |
|-----------|----------------|--------|----------|
| Fixed Window | Poor (2x at edges) | O(1) | Low |
| Sliding Log | None | O(n) | Perfect |
| Sliding Window Counter | Smoothed | O(1) | ~99.97% |
| **Token Bucket** | **Configurable** | **O(1)** | **High** |

Token Bucket won because:
1. **Burst tolerance is a feature, not a bug**. Real traffic is bursty. Allowing 10 requests instantly then refilling at 1/sec matches actual usage patterns.
2. **Two intuitive parameters**: bucket size (max burst) and refill rate (sustained throughput).
3. **Battle-tested**: Used by AWS, Stripe, and most API gateways.

## The Race Condition Problem

Here's the naive implementation that breaks under load:

```go
tokens := redis.Get("tokens")      // Read: 5 tokens
if tokens > 0 {
    redis.Set("tokens", tokens-1)  // Write: 4 tokens
    allowRequest()
}
```

Two concurrent requests both read 5, both write 4. You've allowed 2 requests but only decremented once.

### Solution: Lua Scripts

Redis executes Lua scripts atomically. The entire read-modify-write happens in a single operation:

```lua
local tokens = redis.call('HGET', key, 'tokens')
local last_refill = redis.call('HGET', key, 'last_refill')

-- Initialize on first request
if tokens == nil then
    tokens = bucket_size
    last_refill = now
end

-- Refill based on elapsed time
local elapsed = now - last_refill
tokens = math.min(bucket_size, tokens + elapsed * refill_rate)

-- Consume token if available
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
end

redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('EXPIRE', key, 3600)

return {allowed, tokens, retry_after}
```

This script executes in ~0.1ms and guarantees correctness regardless of concurrency.

## Failure Mode: Fail-Open

When Redis is unavailable, we have two choices:

1. **Fail-closed**: Reject all requests (safe but causes outages)
2. **Fail-open**: Allow all requests (available but unprotected)

We chose fail-open:

```go
result, err := limiter.Allow(ctx, clientKey)
if err != nil {
    log.Printf("Rate limiter error (failing open): %v", err)
    w.Header().Set("X-RateLimit-Warning", "rate-limiter-unavailable")
    proxy.ServeHTTP(w, r)  // Forward request anyway
    return
}
```

**Rationale**: For most APIs, a few seconds without rate limiting is better than a full outage. The warning header lets clients know protection is degraded.

If your threat model requires fail-closed (e.g., billing APIs), flip the behavior.

## Client Identification

Requests are bucketed by client IP:

```go
func getClientIP(r *http.Request) string {
    if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
        return xff
    }
    if xri := r.Header.Get("X-Real-IP"); xri != "" {
        return xri
    }
    return r.RemoteAddr
}
```

Redis key format: `ratelimit:192.168.1.1`

For authenticated APIs, you'd use user ID or API key instead.

## Response Headers

Clients need visibility into their rate limit status:

```
HTTP/1.1 200 OK
X-RateLimit-Limit: 10
X-RateLimit-Remaining: 7

HTTP/1.1 429 Too Many Requests
X-RateLimit-Limit: 10
X-RateLimit-Remaining: 0
X-RateLimit-Retry-After: 3
```

These follow the emerging [IETF draft standard](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).

## Project Structure

```
rate-limiter/
├── gateway/
│   ├── main.go                     # HTTP server, middleware, reverse proxy
│   └── ratelimiter/
│       └── token_bucket.go         # Token bucket algorithm + Lua script
├── backend/
│   └── main.go                     # Mock upstream service
├── tests/
│   └── integration_test.go         # 10 integration test cases
├── scripts/
│   ├── cluster-setup.sh            # Redis cluster creation (6 nodes)
│   └── failover-demo.sh            # Automatic failover demonstration
├── run.sh                          # Main control script
└── README.md                       # This file
```

## API Endpoints

### Gateway (`:8080`)

| Endpoint | Method | Rate Limited | Description |
|----------|--------|--------------|-------------|
| `/health` | GET | No | Gateway health check |
| `/api/resource` | GET | Yes | Fetch resource from backend |
| `/api/resource` | POST | Yes | Create/update resource |
| `/*` | Any | Yes | All other paths proxied to backend |

### Backend (`:8081`)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Returns `{"status": "ok"}` |
| `/api/resource` | GET | Returns sample resource JSON |
| `/api/resource` | POST | Echoes request body |

## Developer Guide

### Prerequisites

- Go 1.21+
- Redis 7.0+ (for standalone mode)
- Bash 4.0+

```bash
# macOS
brew install go redis

# Ubuntu/Debian
sudo apt-get install golang-go redis-server

# Start Redis (standalone)
brew services start redis       # macOS
sudo systemctl start redis      # Linux
```

### Quick Start

```bash
# Clone and run demo
git clone <repo-url>
cd rate-limiter
./run.sh demo
```

### Available Commands

| Command | Description |
|---------|-------------|
| `./run.sh` | Build and run services (keeps running until Ctrl+C) |
| `./run.sh demo` | Run services + send 12 test requests |
| `./run.sh test` | Run all integration tests |
| `./run.sh help` | Show all available commands |
| `./run.sh cluster-start` | Create 6-node Redis cluster |
| `./run.sh cluster-stop` | Tear down Redis cluster |
| `./run.sh cluster-status` | Check cluster health |
| `./run.sh cluster-demo` | Run failover demonstration |

### Running in Standalone Mode

```bash
# Start Redis
redis-server --daemonize yes

# Run demo (sends 12 requests, first 10 succeed, last 2 get 429)
./run.sh demo
```

Output:
```
Request 1: 200 OK (remaining: 9)
Request 2: 200 OK (remaining: 8)
...
Request 10: 200 OK (remaining: 0)
Request 11: 429 Too Many Requests (retry after: 1s)
Request 12: 429 Too Many Requests (retry after: 2s)
```

### Running in Cluster Mode

```bash
# Start 6-node Redis cluster (3 masters + 3 replicas)
./run.sh cluster-start

# Run services with cluster
REDIS_MODE=cluster ./run.sh demo

# Run failover demo (kills a master, watches replica promotion)
./run.sh cluster-demo

# Check cluster health
./run.sh cluster-status

# Stop cluster when done
./run.sh cluster-stop
```

### Manual Testing

```bash
# Terminal 1: Start backend
cd backend && go run .

# Terminal 2: Start gateway
cd gateway && go run .

# Terminal 3: Send requests
curl -v http://localhost:8080/api/resource

# Test with specific client IP
curl -H "X-Forwarded-For: test-client" http://localhost:8080/api/resource

# Check rate limit headers
curl -s -D - http://localhost:8080/api/resource | grep X-RateLimit

# Inspect Redis state directly
redis-cli HGETALL ratelimit:127.0.0.1
redis-cli DEL ratelimit:127.0.0.1  # Reset a client's bucket
```

### Building

```bash
# Build all components
cd gateway && go build -o gateway .
cd ../backend && go build -o backend .

# Run tests with verbose output
cd tests && go test -v ./...
```

## Test Coverage

```bash
./run.sh test
```

| Test | What It Validates |
|------|-------------------|
| `TestRequestsWithinLimit` | N requests under limit all succeed |
| `TestRequestsExceedLimit` | Request N+1 gets 429 |
| `TestTokenRefill` | Waiting restores capacity |
| `TestBurstAllowed` | Full bucket consumed in parallel |
| `TestConcurrentRequests` | No race conditions under load |
| `TestDifferentClients` | Per-client isolation |
| `TestRateLimitHeaders` | Correct header values |
| `TestBackendResponsePassthrough` | Proxy works correctly |

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BUCKET_SIZE` | 10 | Maximum burst capacity (tokens) |
| `REFILL_RATE` | 1.0 | Tokens restored per second |
| `REDIS_MODE` | standalone | Redis mode: `standalone` or `cluster` |
| `REDIS_ADDR` | localhost:6379 | Redis address (standalone mode) |
| `REDIS_ADDRS` | localhost:7000,localhost:7001,localhost:7002 | Redis addresses (cluster mode, comma-separated) |
| `BACKEND_URL` | http://localhost:8081 | Upstream service URL |

### Example Configurations

```bash
# Standalone (development)
BUCKET_SIZE=10 REFILL_RATE=1.0 ./run.sh

# Cluster (production-like)
REDIS_MODE=cluster BUCKET_SIZE=100 REFILL_RATE=10.0 ./run.sh

# Custom backend
BACKEND_URL=http://api.example.com:3000 ./run.sh
```

## Performance Characteristics

- **Latency overhead**: 1-2ms (Redis RTT + Lua execution)
- **Memory per client**: ~100 bytes (hash with 2 fields + key)
- **Scaling**: 10M clients ≈ 1GB Redis memory
- **Throughput**: Limited by Redis; single instance handles 100k+ ops/sec

## Production Considerations

**Redis HA**: Use Redis Sentinel or Redis Cluster. The Lua script is compatible with both.

**Multi-region**: Each region needs its own Redis. For global rate limits, consider:
- Async replication with eventual consistency
- CRDTs (conflict-free replicated data types)
- Accept some over-limit requests during partition

**Monitoring**: Track these metrics:
- `rate_limit_allowed_total` / `rate_limit_rejected_total`
- `rate_limit_latency_ms` (p50, p99)
- Redis connection errors

**Key expiration**: Buckets expire after 1 hour of inactivity. Adjust based on your cardinality.

## What We Didn't Build

- **Tiered limits**: Different limits for different API endpoints
- **Quota management**: Daily/monthly limits with reset
- **Distributed rate limiting**: Coordinating limits across regions
- **Dynamic configuration**: Changing limits without restart

These are left as exercises—or future blog posts.

## Additional Documentation

- **[Redis Developer Guide](docs/REDIS_GUIDE.md)**: Deep dive into Redis internals, Lua scripting, cluster configuration, debugging commands, and production considerations.

## References

- [Stripe: Rate limiters and load shedders](https://stripe.com/blog/rate-limiters)
- [Cloudflare: How we built rate limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/)
- [Google Cloud: Rate limiting strategies](https://cloud.google.com/architecture/rate-limiting-strategies-techniques)
- [Redis: EVAL command documentation](https://redis.io/commands/eval/)
