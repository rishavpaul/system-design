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

## System Design Concepts

This implementation demonstrates several fundamental distributed systems concepts:

### 1. Horizontal Scalability

**Gateway Layer**: Stateless design allows unlimited horizontal scaling
- Multiple gateway instances share the same Redis cluster
- No coordination needed between gateways
- Add capacity by deploying more gateway instances
- Load balancer distributes traffic across gateways

**Data Layer**: Redis Cluster provides horizontal data scaling
- Data sharded across multiple master nodes using consistent hashing
- Each shard handles 1/N of the total client base
- Add shards by resharding the cluster (Redis handles slot migration)

### 2. Partitioning Strategy: Hash-Based Sharding

**Algorithm**: `CRC16(key) % 16384 = hash_slot`
- 16,384 total hash slots distributed across master nodes
- Deterministic: same key always routes to same shard
- Uniform distribution: keys spread evenly across shards

**Benefits**:
- **Data locality**: All operations for a client hit one shard (low latency)
- **No hotspots**: Traffic distributed evenly (assuming diverse client IPs)
- **Independent shards**: No cross-shard coordination (high throughput)

**Trade-offs**:
- Cannot do global aggregations without fan-out queries
- Resharding requires slot migration (temporary performance impact)
- Hash collisions rare but possible (multiple clients on same shard)

### 3. Replication and High Availability

**Master-Replica Architecture**:
- Each master has 1 replica (configurable)
- Asynchronous replication: writes to master, replicated to replica
- Replicas can serve reads (`ReadOnly: true` in cluster config)

**Automatic Failover**:
- If master fails, replica promoted to master (typically 1-2 seconds)
- Redis Cluster uses Raft-like consensus for failover decisions
- Majority of masters must agree on failover (prevents split-brain)

**Consistency Model**: **Eventually consistent reads**
- Writes go to master (strong consistency for writes)
- Reads can come from replicas (may be slightly behind master)
- Trade-off: Read scaling vs. read-after-write consistency
- For rate limiting, eventual consistency is acceptable (slight over-limit OK)

### 4. CAP Theorem Trade-offs

In the face of network partitions, this system chooses **Availability over Consistency** (AP system):

**During normal operation**:
- ✓ Consistency: All gateways see same rate limit state (via Redis)
- ✓ Availability: All gateways can serve requests
- ✓ Partition tolerance: N/A (no partition)

**During Redis failure** (fail-open strategy):
- ✗ Consistency: Gateways cannot coordinate rate limits
- ✓ Availability: Requests still processed (degraded mode)
- ⚠️ Trade-off: Temporary lack of rate limiting vs. full outage

**During network partition** (if Redis Cluster splits):
- Minority partition: Cannot achieve quorum, read-only mode
- Majority partition: Continues operating normally
- Trade-off: Minority partition sacrifices availability for consistency

### 5. Single Point of Failure Analysis

**Standalone Mode SPOFs**:
- ❌ Redis instance failure → All rate limiting lost (fail-open saves availability)
- ✓ Gateway failure → Other gateways continue serving
- ✓ Backend failure → Gateway returns 502 (isolated failure)

**Cluster Mode Resilience**:
- ✓ One master fails → Replica promotes, <2s downtime for that shard
- ✓ One replica fails → Master continues, reads slightly slower
- ⚠️ Majority of masters fail → Cluster becomes read-only
- ✓ All replicas fail → Masters continue (no read scaling)

**Production Recommendations**:
- Use Redis Cluster (minimum 3 masters) for HA
- Deploy gateways across multiple availability zones
- Use Redis Sentinel or managed Redis (AWS ElastiCache, etc.)
- Monitor Redis cluster health continuously

### 6. Consistency Guarantees

**Per-client strong consistency**:
- Lua script executes atomically on one shard
- Read-modify-write is a single operation
- No race conditions even with concurrent requests
- **Guarantee**: Client never exceeds rate limit (unless Redis fails)

**Cross-client eventual consistency**:
- Clients on different shards are independent
- Failover may cause brief inconsistency during replica promotion
- **Acceptable**: Rate limiting is per-client, not global

**Edge case - Redis cluster failover**:
- If master fails before replicating last writes, those writes are lost
- Client might get a few extra requests during failover window
- **Mitigation**: Use `wait` command for critical writes (adds latency)

### 7. Network Partition Handling

**Scenario**: Gateway can't reach Redis

```go
result, err := limiter.Allow(ctx, clientKey)
if err != nil {
    // Fail-open: allow request, add warning header
    w.Header().Set("X-RateLimit-Warning", "rate-limiter-unavailable")
    proxy.ServeHTTP(w, r)
    return
}
```

**Design choice: Fail-open** (prioritize availability)
- Alternative: Fail-closed (prioritize security/billing)
- Use case dependent: API gateway → fail-open, payment API → fail-closed

**Scenario**: Redis Cluster split-brain (network partition between nodes)

Redis Cluster's consensus mechanism prevents split-brain:
- Requires majority of masters to be reachable
- Minority partition enters `CLUSTERDOWN` state
- Only majority partition continues serving writes

### 8. Scalability Limits

| Component | Bottleneck | Limit | Mitigation |
|-----------|-----------|-------|------------|
| Gateway | CPU (proxy overhead) | ~10k req/sec per instance | Horizontal scaling |
| Redis (standalone) | Single-threaded | ~100k ops/sec | Use Redis Cluster |
| Redis Cluster | Network I/O | ~1M ops/sec (3 masters) | Add more masters |
| Memory | Client cardinality | ~10k clients/GB | TTL expiration, LRU eviction |

**Real-world scaling example**:
- 1 million unique clients/day
- 100 requests/client/day
- Peak: 10k req/sec
- **Architecture**: 3 gateway instances + 3-master Redis cluster

### 9. Latency Analysis

**Latency breakdown** (per request):
1. Client → Gateway: ~1-5ms (network RTT)
2. Gateway rate limit check → Redis: ~0.5-1ms (LAN RTT + Lua exec)
3. Gateway → Backend: ~10-50ms (depends on backend)
4. Backend → Client: ~1-5ms (network RTT)

**Total**: ~12-61ms (rate limiter adds <2ms)

**Optimization strategies**:
- Colocate gateway and Redis (same datacenter/AZ)
- Use Redis Cluster read replicas for read scaling
- Connection pooling (already done by go-redis)
- Pipeline multiple Redis commands (not applicable here - single command)

### 10. Data Locality and Cache Efficiency

**Key insight**: All data for a client lives on one Redis shard

**Benefits**:
- Single roundtrip per request (no multi-get)
- CPU cache friendly (same shard serves repeated requests)
- No distributed transactions (avoid 2PC overhead)

**Example**:
```
Client 1.1.1.1 → shard 1 (always)
Client 2.2.2.2 → shard 2 (always)
Client 3.3.3.3 → shard 3 (always)
```

This is **much faster** than:
- Global counter requiring consensus (Raft, Paxos)
- Multi-shard aggregation (fan-out queries)
- Distributed lock acquisition (ZooKeeper, etcd)

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
