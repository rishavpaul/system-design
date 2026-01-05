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
- Connection pooling (already done by go-redis - see below)
- Pipeline multiple Redis commands (not applicable here - single command)

**Connection Pooling Deep Dive**:

Without pooling (naive approach):
```go
// BAD: Creates new connection per request
func handleRequest() {
    client := redis.NewClient(...)  // TCP handshake: ~1ms
    client.Get("key")                // Redis command: ~1ms
    client.Close()                   // Close connection
}
// Total: ~2ms (50% overhead from connection setup/teardown)
```

With pooling (go-redis default):
```go
// GOOD: Reuses connections from pool
client := redis.NewClient(&redis.Options{
    Addr:         "localhost:6379",
    PoolSize:     100,        // Max 100 connections
    MinIdleConns: 10,         // Keep 10 warm (no handshake delay)
    MaxRetries:   3,
})

func handleRequest() {
    client.Get("key")  // Borrows from pool: ~0ms overhead
}
// Total: ~1ms (connection already established)
```

**Why it matters at scale**:

| Metric | Without Pool | With Pool (100 conns) |
|--------|--------------|----------------------|
| Latency per request | 2ms | 1ms (50% faster) |
| Throughput (1 gateway) | ~5k req/sec | ~10k req/sec (2x) |
| Gateway CPU usage | High (TCP handshakes) | Low (reuse conns) |
| Redis connection count | Unbounded (leak risk) | Bounded (100 max) |
| Connection exhaustion | Likely at 10k+ req/sec | Never (pooled) |

**At 100k requests/sec** (10 gateway instances):
- Without pool: 100k TCP handshakes/sec → Gateway CPUs saturated, Redis overwhelmed
- With pool: 1,000 stable connections (10 gateways × 100 pool size) → Smooth operation

**Our configuration** (gateway/main.go):
```go
redisClient = redis.NewClusterClient(&redis.ClusterOptions{
    Addrs:        addrs,
    PoolSize:     100,              // Default: 10*runtime.NumCPU()
    MinIdleConns: 10,               // Pre-warmed for instant availability
    MaxRetries:   3,                // Retry on transient failures
    DialTimeout:  2 * time.Second,  // Connection establishment timeout
    ReadTimeout:  1 * time.Second,  // Per-command timeout
})
// Pool automatically:
// - Creates connections on demand (up to PoolSize)
// - Reuses idle connections (FIFO queue)
// - Closes stale connections (after 5min idle)
// - Health checks connections (periodic PING)
```

**Connection lifecycle**:
```
Request 1 → Get conn from pool → Execute command → Return conn to pool
Request 2 → Reuse same conn     → Execute command → Return conn to pool
(No TCP handshake, immediate execution)
```

**Critical for Redis Cluster**: Each gateway maintains `PoolSize` connections **per shard**:
- 3 shards × 100 pool size = **300 total connections per gateway**
- 10 gateways = **3,000 total cluster connections**
- Redis limit: `maxclients 10000` (default) → Still safe

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

## Redis Cluster Deep Dive

This section answers critical questions about Redis Cluster's behavior, performance, and failure modes.

### Q1. Why does replica promotion take 1-2 seconds?

When a master node fails, the promotion process involves multiple steps:

**1. Failure Detection** (~1 second)
```
Every Redis node sends PING to other nodes every 1 second
If node doesn't respond within cluster-node-timeout (default 5s in our setup):
  - Node is marked PFAIL (Possible Failure) by the observing node
```

**2. Failure Propagation** (~500ms)
```
Nodes gossip about PFAIL state
When majority of masters mark a node as PFAIL:
  - Node is marked FAIL (confirmed failure)
  - Gossip protocol spreads this to all nodes
```

**3. Replica Election** (~300ms)
```
Replicas of the failed master start election:
1. Each replica waits: delay = 500ms + random(0-500ms) + rank_offset
   - rank_offset ensures replica with freshest data gets priority
2. Replica broadcasts FAILOVER_AUTH_REQUEST to all masters
3. Masters vote (can only vote once per epoch)
4. Replica needs majority: (num_masters / 2) + 1 votes
5. Winner broadcasts promotion to cluster
```

**4. Cluster Reconfiguration** (~200ms)
```
All nodes update their cluster state:
- Promoted replica now owns the master's hash slots
- Clients receive MOVED redirects to new master
- go-redis client automatically updates slot cache
```

**Why can't it be instant?**
- **CAP theorem**: Must achieve consensus (partition tolerance + consistency)
- **Network delays**: Gossip protocol needs time to propagate across nodes
- **False positive prevention**: Must distinguish network hiccup from actual failure
- **Split-brain prevention**: Need majority vote to avoid dual masters

**Tuning trade-offs**:
```bash
# Faster failover (risky - more false positives)
cluster-node-timeout 3000  # 3 seconds

# More conservative (slower failover, fewer false positives)
cluster-node-timeout 15000 # 15 seconds
```

### Q2. Redis Replication: Asynchronous by Default

**Standard replication flow**:

```
Client → Master: SET ratelimit:1.1.1.1 tokens=5
Master → Client: OK (immediate response)
Master → Replica: Async replication stream (happens in background)
```

**Timeline**:
```
t=0ms:   Master receives write
t=1ms:   Master applies write to memory
t=2ms:   Master responds OK to client
t=5ms:   Replica receives replication stream
t=6ms:   Replica applies write (now consistent)
```

**Consistency guarantee**: **None** (by default)
- Client can read from replica at t=3ms and see stale data
- If master crashes at t=4ms, that write is lost forever

**Synchronous replication with WAIT command**:

```lua
-- Lua script modification for stronger consistency
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)

-- WAIT blocks until N replicas acknowledge the write
-- Returns: number of replicas that acknowledged (0-N)
local num_acked = redis.call('WAIT', 1, 1000)  -- Wait for 1 replica, timeout 1000ms

if num_acked < 1 then
    -- Replication failed or timed out
    -- Options:
    -- 1. Return error (fail-closed, strong consistency)
    -- 2. Continue anyway (fail-open, accept risk)
    return redis.error_reply("Replication timeout")
end

return {allowed, tokens, retry_after}
```

**How WAIT actually works**:

The Lua script **cannot directly communicate with replicas**. Here's the actual architecture:

```
┌─────────────────────────────────────────────────────────┐
│  Redis Master                                           │
│                                                         │
│  1. Lua script executes HSET                            │
│     └─> Write applied to master's memory               │
│         (replication offset incremented)                │
│                                                         │
│  2. Lua script calls WAIT(1, 1000)                      │
│     └─> Lua blocks, control returns to Redis           │
│         Redis enters "waiting for replication" state    │
│                                                         │
│  3. Redis's replication thread sends write to replicas  │
│     ├─> Replica 1 (async, via TCP socket)              │
│     ├─> Replica 2 (async, via TCP socket)              │
│     └─> Replica 3 (async, via TCP socket)              │
│                                                         │
│  4. Replicas send ACK back to master                    │
│     Format: REPLCONF ACK <offset>                       │
│     ├─> Replica 1: ACK offset=1234                      │
│     ├─> Replica 2: ACK offset=1234 ✓ (first ACK!)      │
│     └─> Replica 3: ACK offset=1230 (lagging)            │
│                                                         │
│  5. Master receives ACK from Replica 2                  │
│     └─> Count: 1 replica acknowledged                   │
│         Threshold met (needed 1)!                       │
│         Resume Lua script execution                     │
│                                                         │
│  6. Lua script resumes, WAIT returns 1                  │
│     └─> Script continues with return statement         │
└─────────────────────────────────────────────────────────┘

Total time: ~5ms (network RTT + replica processing)
```

**WAIT is not a Lua-level API** - it's a Redis command that:
1. Tells Redis: "Don't let Lua script continue until N replicas confirm"
2. Redis manages all replica communication (Lua is oblivious)
3. Redis tracks replication offsets to know which replicas are up-to-date
4. Returns count of replicas that acknowledged (not explicit "ack" objects)

**WAIT command details**:

```redis
WAIT <numreplicas> <timeout>

Arguments:
- numreplicas: Minimum replicas that must acknowledge (1-N)
- timeout: Max wait time in milliseconds (0 = wait forever)

Returns:
- Integer: Number of replicas that acknowledged
  - 0 = timeout expired, no replicas acked
  - 1 = 1 replica acked
  - N = all N replicas acked

Example:
> SET key value
OK
> WAIT 2 1000
(integer) 2   ← 2 replicas acknowledged within 1 second

> SET key value
OK
> WAIT 5 1000
(integer) 3   ← Only 3 replicas acked (maybe only 3 exist, or 2 are slow)
```

**Why Lua can't directly get replica ACKs**:

Lua scripts run in Redis's single-threaded execution context:
- Lua has **no network access** (security/safety)
- Lua has **no threading** (can't spawn async tasks)
- Lua can only call **Redis commands** (limited API)
- **Replication is managed by Redis**, not exposed to Lua

**Under the hood: Replication protocol**:

```
Master → Replica communication:
1. Master sends: PING (heartbeat every 1s)
2. Replica responds: REPLCONF ACK <offset>
   - offset = position in replication stream

When WAIT is called:
1. Master records current replication offset (e.g., 12345)
2. Master waits for replicas to send ACK >= 12345
3. Each replica sends: REPLCONF ACK 12345 (or higher)
4. Master counts ACKs, resumes when threshold met
```

**Important: WAIT doesn't guarantee durability**

```
Scenario: WAIT succeeds, but data still lost

t=0ms:   Client: SET key value
t=1ms:   Master: Write to memory, offset=1000
t=2ms:   Master: WAIT 1 1000
t=5ms:   Replica: Receives write, applies to memory, offset=1000
t=6ms:   Replica → Master: REPLCONF ACK 1000
t=7ms:   Master: WAIT returns 1 (success!)
t=8ms:   Client receives OK
t=9ms:   Replica crashes ❌ (no persistence!)
t=10ms:  Master crashes ❌
Result:  Write lost despite WAIT success

Why? Both master and replica only wrote to MEMORY, not disk!
```

**For true durability, combine WAIT + fsync**:

```lua
-- Maximum durability (slowest)
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('WAIT', 1, 1000)  -- Wait for 1 replica (memory)
-- At this point, master AND 1 replica have data in memory

-- Replica must have AOF enabled with appendfsync always:
-- appendfsync always  (replica config)
-- This forces replica to fsync to disk on every write
```

**Redis persistence options**:

| Persistence | Durability | Performance |
|-------------|------------|-------------|
| None | Lost on crash | Fastest |
| AOF (appendfsync everysec) | Last ~1s lost | Fast |
| AOF (appendfsync always) | Zero loss | Slow (~100x slower) |
| WAIT 1 + AOF (everysec) | Survives master crash | Moderate |
| WAIT 1 + AOF (always) | Survives master+replica crash | Slowest |

**Trade-off**:
- ✓ WAIT ensures replication to replica memory (survives master failure)
- ✗ Doesn't guarantee disk persistence (both can crash simultaneously)
- ✗ 2-5x higher latency (wait for replica acknowledgment)
- ✗ Reduced throughput (master blocks on replication)
- ✗ Can still timeout (replica unreachable or slow)

**Why we use async replication**:
- Rate limiting tolerates slight inconsistency (a few extra requests during failover is acceptable)
- Low latency is critical (every 1ms matters at edge)
- Throughput matters more than perfect accuracy

**Production consideration**:
```go
// For critical operations (billing, quota), use WAIT
// For rate limiting, accept async replication risk
```

### Q3. Redis Architecture at Scale

**Single Node Architecture**:

```
┌─────────────────────────────────────┐
│         Redis Server                │
│                                     │
│  ┌───────────────────────────────┐ │
│  │  Main Thread (Event Loop)     │ │  ← Single-threaded!
│  │  - Accept connections          │ │
│  │  - Parse commands              │ │
│  │  - Execute commands (Lua too)  │ │
│  │  - Build responses             │ │
│  └───────────────────────────────┘ │
│                                     │
│  ┌───────────────────────────────┐ │
│  │  I/O Threads (Redis 6.0+)     │ │  ← Multi-threaded I/O
│  │  - Socket reads/writes         │ │
│  │  - Protocol parsing            │ │
│  │  - Response serialization      │ │
│  └───────────────────────────────┘ │
│                                     │
│  ┌───────────────────────────────┐ │
│  │  Background Threads            │ │
│  │  - AOF fsync                   │ │
│  │  - RDB snapshots               │ │
│  │  - Lazy key deletion           │ │
│  └───────────────────────────────┘ │
│                                     │
│  ┌───────────────────────────────┐ │
│  │  In-Memory Data Structure     │ │
│  │  - Hash tables                 │ │
│  │  - Skip lists                  │ │
│  │  - Linked lists                │ │
│  └───────────────────────────────┘ │
└─────────────────────────────────────┘
```

**Redis Cluster Architecture** (3 masters + 3 replicas):

```
                    ┌──────────────────────────────┐
                    │   Cluster Bus (Port +10000)  │
                    │   Gossip Protocol Exchange   │
                    │   - Heartbeats every 1s      │
                    │   - Cluster state sync       │
                    │   - Failure detection        │
                    └──────────────────────────────┘
                               ↕
        ┌─────────────────────────────────────────────┐
        │                                             │
   ┌────▼─────┐         ┌──────────┐         ┌──────▼─────┐
   │ Master 1 │────────▶│ Replica 1│         │  Master 2  │
   │ :7000    │  async  │ :7003    │         │  :7001     │
   │ Slots:   │  repl   │          │         │  Slots:    │
   │ 0-5460   │         │          │         │  5461-10922│
   └──────────┘         └──────────┘         └─────┬──────┘
                                                   │ async
                                             ┌─────▼──────┐
   ┌──────────┐         ┌──────────┐         │ Replica 2  │
   │ Master 3 │────────▶│ Replica 3│         │ :7004      │
   │ :7002    │  async  │ :7005    │         │            │
   │ Slots:   │  repl   │          │         └────────────┘
   │10923-    │         │          │
   │ 16383    │         │          │
   └──────────┘         └──────────┘

Gateway calculates: CRC16(key) % 16384 = slot
Then routes to master owning that slot
```

**Gossip Protocol Details**:

Every node maintains:
- **Cluster state**: All nodes, their slots, master/replica role
- **Heartbeat**: Sends PING to random nodes every 1 second
- **State propagation**: Gossips latest cluster changes

```
Node A: "I think Master 1 is down (PFAIL)"
  ↓ (gossip)
Node B: "I also think Master 1 is down (PFAIL)"
  ↓ (gossip)
Node C: "I also think Master 1 is down (PFAIL)"
  → Majority reached: Master 1 marked FAIL
  → Trigger replica promotion
```

**Scaling strategy**:

| Setup | Masters | Replicas | Hash Slots per Master | Max Throughput |
|-------|---------|----------|----------------------|----------------|
| Small | 3 | 3 | ~5,461 | ~300k ops/sec |
| Medium | 6 | 6 | ~2,731 | ~600k ops/sec |
| Large | 12 | 12 | ~1,365 | ~1.2M ops/sec |
| Huge | 24 | 24 | ~682 | ~2.4M ops/sec |

**Resharding process** (adding capacity):
1. Add new master nodes to cluster
2. Redistribute hash slots: `redis-cli --cluster reshard`
3. Migrate keys slot-by-slot (gradual, no downtime)
4. Update client slot cache

### Q4. Throughput Limits and Master Election

**Throughput Breakdown**:

**Single Redis instance** (1 master, 0 replicas):
- **Theoretical max**: ~100,000 ops/sec (single-threaded command processing)
- **Realistic**: ~80,000 ops/sec (accounting for network overhead)
- **Bottleneck**: Single-threaded event loop, CPU-bound

**Redis Cluster** (3 masters, 3 replicas):
- **Theoretical max**: ~300,000 ops/sec (3x single instance)
- **Realistic**: ~250,000 ops/sec (accounting for cluster overhead)
- **Bottleneck**: Network I/O, CPU (splits across shards)

**Redis Cluster** (10 masters, 10 replicas):
- **Theoretical max**: ~1,000,000 ops/sec
- **Realistic**: ~800,000 ops/sec
- **Bottleneck**: Network bandwidth, gossip protocol overhead

**For our rate limiter**:
```
Assumptions:
- 1 operation per request (Lua script execution)
- 3-master cluster
- 250k ops/sec capacity

Max requests/sec: 250,000 req/sec (distributed across 3 shards)
```

**Master Election Algorithm** (Raft-like consensus):

```
1. Master failure detected (FAIL state)
   └─ All replicas of that master become candidates

2. Election delay calculation (prioritizes freshest replica):
   delay = 500ms + random(0, 500ms) + (rank * 1000ms)

   rank = position in replication offset list
   - rank 0: Most up-to-date replica (lowest delay)
   - rank 1: Second most up-to-date (higher delay)

   Example:
   - Replica A (offset 1000): delay = 500 + 200 + 0 = 700ms
   - Replica B (offset 900):  delay = 500 + 300 + 1000 = 1800ms
   → Replica A requests votes first

3. Voting process:
   - Replica broadcasts FAILOVER_AUTH_REQUEST to all masters
   - Each master can vote once per epoch
   - Master votes YES if:
     * Replica's master is marked FAIL
     * Replica's epoch is current
     * Master hasn't voted this epoch
   - Replica needs majority: (N/2) + 1 votes

   Example (3 masters total):
   - Needs 2 votes to win
   - Replica A gets votes from Master 2, Master 3 → WINS
   - Replica B times out waiting for votes

4. Promotion:
   - Winner broadcasts FAILOVER_AUTH_ACK (I am the new master)
   - All nodes update cluster state
   - Former replica now owns failed master's slots
   - Epoch increments (prevents stale elections)
```

**Why Raft-like, not pure Raft?**
- Redis Cluster optimizes for availability over strong consistency
- No persistent log (Raft requires durable log)
- Weaker guarantees (async replication can lose writes)
- Faster failover (1-2s vs 5-10s in typical Raft)

### Q5. Threading Model and Lua Script Efficiency

**Redis Threading Model**:

**Before Redis 6.0**: Pure single-threaded
```
One thread does everything:
- Read from socket
- Parse command
- Execute command
- Write response
```

**Redis 6.0+**: Multi-threaded I/O (our implementation)
```
Main thread:
- Parse commands
- Execute commands (including Lua)
- Coordinate I/O threads

I/O threads (configurable, default 4):
- Read from sockets (parallel)
- Write to sockets (parallel)
- Protocol parsing/serialization

Main thread still single-threaded for execution!
```

**Why single-threaded execution?**

✓ **Pros**:
- No locks needed (zero contention overhead)
- Atomic operations guaranteed (Lua scripts run exclusively)
- Predictable performance (no thread scheduling jitter)
- Simple mental model (no race conditions)

✗ **Cons**:
- CPU bound (can't use all cores for execution)
- Long-running commands block everything
- Max throughput ~100k ops/sec per instance

**Lua Script Execution**:

```
┌─────────────────────────────────────────┐
│  Redis Main Thread                      │
│                                         │
│  1. Client sends Lua script             │
│  2. Redis locks event loop ─────┐       │
│  3. Execute Lua script          │ BLOCKING!
│  4. Unlock event loop ──────────┘       │
│  5. Return result to client             │
│                                         │
│  Other clients must wait during 2-4     │
└─────────────────────────────────────────┘
```

**Our token bucket Lua script performance**:

```lua
-- This script takes ~0.1ms to execute
local tokens = redis.call('HGET', key, 'tokens')           -- 1 Redis call
local last_refill = redis.call('HGET', key, 'last_refill') -- 1 Redis call
-- Math operations (nanoseconds)
local elapsed = now - last_refill
tokens = math.min(bucket_size, tokens + elapsed * refill_rate)
-- Write back
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now) -- 1 Redis call
redis.call('EXPIRE', key, 3600)                                -- 1 Redis call
return {allowed, tokens, retry_after}
```

**Efficiency analysis**:
- **4 Redis calls**: HGET (2x), HSET, EXPIRE
- **Total execution time**: ~100 microseconds
- **Impact**: Blocks Redis for 0.1ms → other clients wait
- **Throughput**: ~10,000 scripts/sec per shard

**Is this efficient?** ✓ Yes, because:
1. **Short execution time**: 0.1ms is negligible
2. **Atomicity benefit**: Prevents race conditions (worth the blocking cost)
3. **Alternative would be worse**: Multi-round trips would add network latency (1-5ms)
4. **Cluster sharding**: Different clients hit different shards (parallelism)

**When Lua scripts become problematic**:
```lua
-- BAD: This blocks Redis for seconds
for i = 1, 1000000 do
    redis.call('SET', 'key' .. i, i)
end

-- GOOD: Our script does minimal work
-- 4 Redis calls + simple arithmetic = fast
```

**Best practices**:
- Keep Lua scripts under 1ms execution time
- Avoid loops over large datasets
- No I/O operations (HTTP calls, file reads)
- Prefer atomic scripts over multiple round-trips

### Q6. Network Partition Scenarios in Cloud Deployments

**Cloud deployment architecture**:

```
Region: us-east-1

AZ-1a                    AZ-1b                    AZ-1c
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│ Gateway 1   │         │ Gateway 2   │         │ Gateway 3   │
│ Master 1    │         │ Master 2    │         │ Master 3    │
│ Replica 2   │         │ Replica 3   │         │ Replica 1   │
└─────────────┘         └─────────────┘         └─────────────┘
       │                       │                       │
       └───────────────────────┴───────────────────────┘
                    VPC Network (low latency)
```

**Scenario 1: Gateway ↔ Redis partition**

```
┌─────────┐           ╳╳╳╳╳╳╳           ┌─────────┐
│Gateway 1│  Network failure  │ Redis   │
│ (AZ-1a) │     (1% packet    │ Cluster │
│         │      loss)        │         │
└─────────┘                   └─────────┘
```

**What happens**:
1. Gateway's Redis client detects connection timeout (2s in our config)
2. `limiter.Allow()` returns error
3. Gateway enters fail-open mode:
   ```go
   if err != nil {
       log.Printf("Rate limiter error (failing open): %v", err)
       w.Header().Set("X-RateLimit-Warning", "rate-limiter-unavailable")
       proxy.ServeHTTP(w, r)  // Allow request anyway
       return
   }
   ```
4. All requests allowed (no rate limiting)
5. Users get degraded service (no protection against abuse)

**Rate limiter behavior**: ⚠️ **Degraded** (fail-open)
- ✓ Availability maintained (requests still processed)
- ✗ Consistency lost (no rate limit enforcement)
- Users with `X-RateLimit-Warning` header

**Mitigation**:
```go
// Option 1: Client-side rate limiting (fallback)
type LocalRateLimiter struct {
    mu      sync.Mutex
    buckets map[string]*localBucket  // In-memory fallback
}

if err != nil {
    // Try local rate limiter
    if !localLimiter.Allow(clientIP) {
        return 429
    }
    proxy.ServeHTTP(w, r)  // Allow with local limit
}

// Option 2: Multi-region Redis (complexity++)
// Deploy Redis in multiple regions, accept eventual consistency
```

**Scenario 2: Redis Cluster split-brain (AZ partition)**

```
AZ-1a                    Network partition        AZ-1b + AZ-1c
┌─────────────┐         ╳╳╳╳╳╳╳╳╳╳╳╳╳╳╳         ┌─────────────────┐
│ Master 1    │         ╳                        │ Master 2        │
│ Replica 2   │         ╳                        │ Master 3        │
│             │         ╳                        │ Replica 1       │
│ (MINORITY)  │         ╳                        │ Replica 3       │
│             │         ╳                        │ (MAJORITY)      │
└─────────────┘         ╳╳╳╳╳╳╳╳╳╳╳╳╳╳╳         └─────────────────┘
  1 master                                         2 masters
  (can't reach majority)                           (has majority)
```

**What happens**:

**In AZ-1a (minority partition)**:
1. Master 1 can't reach Master 2, Master 3 (gossip fails)
2. Master 1 detects it's in minority partition
3. Master 1 enters **CLUSTERDOWN** state (rejects all writes)
4. Clients get error: `CLUSTERDOWN The cluster is down`
5. Replica 2 can't promote (needs majority of masters to vote)

**In AZ-1b/1c (majority partition)**:
1. Master 2, Master 3 detect Master 1 is unreachable
2. Mark Master 1 as FAIL (gossip consensus)
3. Trigger election for Master 1's replica
4. Replica 1 (in AZ-1c) wins election, promotes to master
5. **Cluster continues operating** with 2 masters + 1 promoted replica

**Rate limiter behavior**:

| Location | Redis State | Rate Limiter Behavior |
|----------|-------------|----------------------|
| AZ-1a | CLUSTERDOWN (minority) | ⚠️ Fail-open (all requests allowed) |
| AZ-1b, 1c | HEALTHY (majority) | ✓ Normal operation |

**Timeline**:
```
t=0s:    Network partition occurs
t=1s:    Nodes detect unreachable peers (gossip timeout)
t=2s:    Majority marks Master 1 as FAIL
t=3s:    Replica 1 promoted to master in majority partition
t=4s:    AZ-1a requests fail → fail-open mode activated

Duration: AZ-1a has 4s of errors, then fail-open until partition heals
```

**When partition heals**:
```
t=60s:   Network partition resolves
t=61s:   Old Master 1 detects cluster state changed
t=62s:   Old Master 1 realizes it's been replaced
t=63s:   Old Master 1 becomes replica of new promoted master
t=64s:   Cluster fully healed (now 2 original masters + 1 promoted master + 3 replicas)
```

**Data loss risk**:
- Writes to old Master 1 during partition (t=0-4s) are lost
- Async replication means those writes never reached replicas
- Clients in AZ-1a may have received "OK" but data disappeared

**Scenario 3: Cross-region partition**

```
Region: us-east-1                     Region: us-west-2
┌─────────────────┐                  ┌─────────────────┐
│ Gateway 1-3     │ ╳╳╳╳╳╳╳╳╳╳╳╳╳╳╳  │ Gateway 4-6     │
│ Redis Cluster A │    Internet      │ Redis Cluster B │
│ (independent)   │    Partition     │ (independent)   │
└─────────────────┘                  └─────────────────┘
```

**Rate limiter behavior**: ⚠️ **Independent limits per region**
- Client in us-east-1: Gets rate limited by Cluster A
- Same client in us-west-2: Gets rate limited by Cluster B
- **Total limit = 2x intended** (inconsistent across regions)

**This is by design** (for low latency):
- Cross-region Redis calls add 50-100ms latency (unacceptable)
- Trade-off: Accept regional inconsistency for performance

**Alternative** (complex):
- Use CRDTs (Conflict-free Replicated Data Types) for global limits
- Cassandra or DynamoDB with global tables
- Accept eventual consistency (hours of propagation delay)

### Q7. Instantaneous Failover: Is It Possible?

**Short answer**: No, due to CAP theorem. But we can get close.

**Fundamental limits**:

```
CAP Theorem: Pick 2 of 3
- Consistency: All nodes see same data
- Availability: System responds to requests
- Partition tolerance: System works despite network failures

For distributed Redis:
✓ Partition tolerance (required in real world)
✓ Availability (fail-open strategy)
✗ Consistency (async replication, eventual consistency)

To achieve instantaneous failover:
✓ Consistency (must know who's the master)
✓ Availability (must respond immediately)
✗ Impossible without partition tolerance
```

**Fastest possible failover approaches**:

**1. Redis Sentinel (faster than Cluster for failover)**

```
┌─────────┐
│Sentinel1│──┐
└─────────┘  │
┌─────────┐  │   Monitor master health
│Sentinel2│──┼──→ Quorum-based failover
└─────────┘  │   Faster election (simpler protocol)
┌─────────┐  │
│Sentinel3│──┘
└─────────┘

Failover time: 500ms - 1s (vs 1-2s for Cluster)
```

Why faster?
- Dedicated sentinel nodes (only monitor, don't serve data)
- Simpler quorum protocol (no slot migration)
- Pre-configured replica (no election delay calculation)

**2. Multiple Replicas (reduces detection time)**

```
Master ─┬─→ Replica 1 (AZ-1a)
        ├─→ Replica 2 (AZ-1b)
        └─→ Replica 3 (AZ-1c)

If Master fails:
- 3 replicas detect failure simultaneously
- Best-positioned replica promotes fastest
- Redundancy reduces false negative risk
```

Trade-off:
- ✓ Faster detection (more observers)
- ✗ Higher cost (3x memory for replicas)

**3. Client-side Caching (avoid failover altogether)**

```go
type CachedRateLimiter struct {
    redis       *ratelimiter.TokenBucket
    localCache  *lru.Cache  // In-memory fallback
}

func (c *CachedRateLimiter) Allow(key string) bool {
    // Try Redis first
    result, err := c.redis.Allow(ctx, key)
    if err == nil {
        c.localCache.Set(key, result)  // Cache result
        return result.Allowed
    }

    // Fallback to cache during Redis failure
    if cached, ok := c.localCache.Get(key); ok {
        log.Println("Using cached rate limit (Redis down)")
        return cached.Allowed  // Instant response!
    }

    // No cache, fail open
    return true
}
```

Trade-offs:
- ✓ Zero failover time (instant fallback to cache)
- ✗ Cache staleness (rate limits may be outdated)
- ✗ Memory overhead (cache in each gateway instance)
- ✗ Inconsistency (different gateways have different cache states)

**4. Pre-warmed Connections (reduce connection overhead)**

```go
// Our implementation already does this (go-redis connection pooling)
redisClient := redis.NewClusterClient(&redis.ClusterOptions{
    Addrs:        addrs,
    PoolSize:     100,              // Pre-create 100 connections
    MinIdleConns: 10,               // Keep 10 always warm
    MaxRetries:   3,                // Retry failed connections
})
```

**5. Read-Your-Writes Consistency (eliminate replication lag)**

```lua
-- Modify Lua script to use WAIT
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('WAIT', 1, 1000)  -- Wait for 1 replica acknowledgment
return {allowed, tokens, retry_after}
```

Trade-offs:
- ✓ Writes survive master failure (zero data loss)
- ✗ 2-5x higher latency (wait for replication)
- ✗ Reduced throughput (blocks during replication)

**Best achievable failover time**:

| Approach | Failover Time | Data Loss | Complexity |
|----------|--------------|-----------|------------|
| Redis Cluster (our setup) | 1-2s | Last ~100ms of writes | Medium |
| Redis Sentinel | 500ms-1s | Last ~50ms of writes | Medium |
| Multiple replicas + WAIT | 500ms-1s | Zero | High |
| Client-side cache | 0ms (instant) | N/A (degraded) | Medium |
| Multi-region active-active | 0ms (instant) | Inconsistent limits | Very High |

**Recommendation for production**:
```
Combine approaches:
1. Redis Cluster (HA foundation)
2. WAIT for critical operations (billing, quotas)
3. Client-side cache (fallback during failures)
4. Monitoring + alerting (detect failures fast)

Result: <500ms failover with minimal data loss
```

### Q8. Adding Nodes and Slot Migration

**Process of adding a new master node**:

**Step 1: Add empty node to cluster**

```bash
# Start new Redis node
redis-server --port 7006 --cluster-enabled yes

# Add to existing cluster
redis-cli --cluster add-node 127.0.0.1:7006 127.0.0.1:7000

# Current state:
# Master 1: slots 0-5460      (5461 slots)
# Master 2: slots 5461-10922  (5462 slots)
# Master 3: slots 10923-16383 (5461 slots)
# Master 4: slots (none)      (0 slots)  ← New node, not serving traffic yet
```

**Step 2: Reshard slots to new node**

```bash
# Redistribute slots evenly
redis-cli --cluster reshard 127.0.0.1:7000 \
    --cluster-from all \                      # Take from all nodes
    --cluster-to <new-node-id> \              # Give to new node
    --cluster-slots 4096                      # Move 4096 slots

# Target distribution (16384 slots / 4 masters = 4096 each):
# Master 1: slots 0-4095      (4096 slots)
# Master 2: slots 4096-8191   (4096 slots)
# Master 3: slots 8192-12287  (4096 slots)
# Master 4: slots 12288-16383 (4096 slots)  ← Now serving 1/4 of traffic
```

**Step 3: Slot migration happens gradually**

```
For each slot being migrated (e.g., slot 5000 from Master 2 → Master 4):

1. Master 4: Mark slot 5000 as IMPORTING from Master 2
2. Master 2: Mark slot 5000 as MIGRATING to Master 4
3. Migrate keys one by one:
   FOR each key in slot 5000:
       MIGRATE 127.0.0.1 7006 key 0 5000
       (Atomically move key to new node)
4. When all keys migrated:
   Broadcast: "Slot 5000 now owned by Master 4"
5. All nodes update their slot map

Repeat for all 4096 slots...
```

**Client experience during migration**:

**Scenario A: Client requests key in completed slot**

```
Client: GET ratelimit:1.1.1.1  (slot 4000, already migrated to Master 4)
  ↓
Gateway calculates: CRC16("ratelimit:1.1.1.1") % 16384 = 4000
  ↓
go-redis client checks slot cache: slot 4000 → Master 4
  ↓
Send request to Master 4
  ↓
Master 4: OK (returns data)

Result: Normal operation, no latency impact
```

**Scenario B: Client requests key in slot being migrated**

```
Client: GET ratelimit:2.2.2.2  (slot 5000, migration in progress)
  ↓
go-redis client thinks: slot 5000 → Master 2 (old mapping)
  ↓
Send request to Master 2
  ↓
Master 2 checks:
  - Is key "ratelimit:2.2.2.2" still here? NO (already migrated)
  - Respond: -ASK 5000 127.0.0.1:7006

go-redis client receives ASK redirect:
  ↓
Send ASKING command to Master 4 (tells it to serve migrating slot)
  ↓
Send GET ratelimit:2.2.2.2 to Master 4
  ↓
Master 4: OK (returns data)
  ↓
go-redis updates slot cache: slot 5000 → Master 4

Result: 1 extra round-trip (ASK redirect)
Latency: +1-2ms for redirected requests
```

**Scenario C: Client requests key that hasn't migrated yet**

```
Client: GET ratelimit:3.3.3.3  (slot 5000, not migrated yet)
  ↓
go-redis sends to Master 2 (current owner)
  ↓
Master 2: Still has this key, returns data normally

Result: Normal operation
```

**Scenario D: Slot ownership changed permanently**

```
Client: GET ratelimit:4.4.4.4  (slot 5000, fully migrated)
  ↓
go-redis sends to Master 2 (stale cache)
  ↓
Master 2: Slot no longer mine!
  - Respond: -MOVED 5000 127.0.0.1:7006

go-redis receives MOVED redirect:
  ↓
Update slot cache: slot 5000 → Master 4
  ↓
Retry request to Master 4
  ↓
Master 4: OK (returns data)

Result: 1 extra round-trip (MOVED redirect), then cached
Subsequent requests go directly to Master 4
```

**Impact on end-user experience**:

| Metric | Impact | Duration |
|--------|--------|----------|
| Latency (P50) | +0.5ms | During migration |
| Latency (P99) | +2ms (redirects) | During migration |
| Error rate | 0% (no errors) | N/A |
| Throughput | -5% (migration overhead) | During migration |
| Availability | 100% | N/A |

**Migration performance**:

```
Keys per slot: ~1000 (assuming 3M keys / 16384 slots)
Migration speed: ~1000 keys/sec per slot
Time per slot: ~1 second
Total time for 4096 slots: ~1 hour

Settings to tune:
--cluster-pipeline <num>         # Parallel migrations (default 10)
--cluster-timeout <ms>           # Migration timeout (default 60000)
```

**Best practices**:

1. **Migrate during low-traffic periods**
   ```bash
   # Schedule migration at 2 AM
   crontab -e
   0 2 * * * redis-cli --cluster reshard ...
   ```

2. **Monitor during migration**
   ```bash
   # Watch migration progress
   redis-cli --cluster check 127.0.0.1:7000

   # Monitor latency
   redis-cli --latency -h 127.0.0.1 -p 7000
   ```

3. **Gradual migration**
   ```bash
   # Move 1000 slots at a time instead of 4096
   for i in {1..4}; do
       redis-cli --cluster reshard ... --cluster-slots 1000
       sleep 300  # Wait 5 minutes between batches
   done
   ```

4. **Client-side caching** (absorb redirect overhead)
   ```go
   // go-redis automatically caches slot mappings
   // Reduce redirect impact by warming cache
   ```

**Rollback scenario** (if migration goes wrong):

```bash
# Stop migration
redis-cli --cluster reshard ... --cluster-slots 0

# Revert slot assignment
redis-cli --cluster reshard 127.0.0.1:7000 \
    --cluster-from <new-node-id> \
    --cluster-to all \
    --cluster-slots 4096

# Remove node
redis-cli --cluster del-node 127.0.0.1:7000 <new-node-id>
```

**Summary**:

Adding nodes is **safe and gradual**:
- ✓ Zero downtime (migrations happen online)
- ✓ No data loss (atomic key migrations)
- ✓ Automatic client adaptation (redirects + slot cache)
- ⚠️ Slight latency increase (~1-2ms) during migration
- ⚠️ Resource overhead (migration consumes CPU/network)

For production:
- Plan migrations during maintenance windows
- Monitor metrics (latency, error rate, redirect count)
- Use gradual migration (small batches)
- Test rollback procedures

### Q9. Why 16,384 Slots? How Does Redis Distribute Them?

**Why exactly 16,384 slots?**

This number wasn't arbitrary. Redis Cluster's designers chose 16,384 (2^14) for specific technical reasons:

**1. Cluster bus bitmap size optimization**

Redis uses the cluster bus (gossip protocol) to propagate cluster state. Each message contains a bitmap indicating which slots a node owns:

```
Bitmap size = 16,384 slots / 8 bits per byte = 2,048 bytes = 2KB

With 1,000 slots:  125 bytes (too granular, more rebalancing)
With 16,384 slots: 2,048 bytes (sweet spot)
With 65,536 slots: 8,192 bytes (too much gossip overhead)
```

**Why 2KB is optimal**:
- Small enough to fit in L1 cache (32-64KB on modern CPUs)
- Heartbeat messages sent every 1 second to random nodes
- With 100 nodes, each node sends ~10 heartbeats/sec = 20KB/sec gossip overhead
- Acceptable network cost for cluster coordination

**2. Maximum cluster size constraint**

Redis Cluster was designed for "medium-scale" deployments:

```
Recommended max: 1,000 nodes (in practice, most use 3-50 nodes)
Theoretical max: ~16,000 nodes (one node per slot)

With 16,384 slots:
- Each node owns at least 1 slot (if ≤16,384 nodes)
- Typical deployment: 3-10 nodes → 1,638-5,461 slots each
- Large deployment: 100 nodes → 163 slots each
```

**Why not more slots?**
- Gossip protocol overhead grows with cluster size (O(N²) communication)
- Each node maintains full cluster state (memory grows linearly)
- Slot migration gets slower (more slots = more migrations)

**3. Hash distribution granularity**

16,384 slots provides excellent hash distribution:

```
CRC16 hash space: 0-65,535 (16 bits)
CRC16(key) % 16,384 = slot (0-16,383)

Example key distribution (1 million keys):
- Expected keys per slot: 1,000,000 / 16,384 ≈ 61 keys
- Actual variance: ±5% (CRC16 has good uniformity)
- More slots → better distribution (diminishing returns after 16K)
```

**4. Historical: Redis protocol efficiency**

Redis Cluster uses RESP (Redis Serialization Protocol):

```
Client slot cache update message:
CLUSTER SLOTS returns:
[
  [0, 5460, ["127.0.0.1", 7000], ["127.0.0.1", 7003]],
  [5461, 10922, ["127.0.0.1", 7001], ["127.0.0.1", 7004]],
  [10923, 16383, ["127.0.0.1", 7002], ["127.0.0.1", 7005]]
]

With 16,384 slots and 3 masters:
- Message size: ~500 bytes (compact)
- Clients can cache this in a simple array (16K * 8 bytes = 128KB)
```

**Trade-off analysis**:

| Slot Count | Pros | Cons |
|------------|------|------|
| 1,024 | Tiny gossip overhead | Poor hash distribution, frequent rebalancing |
| 4,096 | Small messages | Suboptimal key distribution |
| **16,384** | **Balanced** | **Good distribution, reasonable overhead** |
| 65,536 | Perfect distribution | 4x gossip overhead, huge cluster state |
| 1,000,000 | Excellent distribution | Impractical (gossip overhead kills cluster) |

**How Redis distributes slots among nodes**

**Initial cluster creation**:

When you create a cluster with `redis-cli --cluster create`, Redis uses a **greedy round-robin algorithm**:

```bash
redis-cli --cluster create \
  127.0.0.1:7000 127.0.0.1:7001 127.0.0.1:7002 \
  127.0.0.1:7003 127.0.0.1:7004 127.0.0.1:7005 \
  --cluster-replicas 1
```

**Algorithm**:

```
Input: 6 nodes (3 masters, 3 replicas)
Total slots: 16,384

Step 1: Identify masters (first 3 nodes in our case)
  Masters: 7000, 7001, 7002
  Replicas: 7003, 7004, 7005

Step 2: Calculate slots per master
  slots_per_master = 16,384 / 3 = 5,461.33...

  Master 1 gets: 5,461 slots
  Master 2 gets: 5,461 slots
  Master 3 gets: 5,462 slots (gets the remainder)

Step 3: Assign contiguous slot ranges (for simplicity)
  Master 1 (7000): slots 0-5460      (5,461 slots)
  Master 2 (7001): slots 5461-10921  (5,461 slots)
  Master 3 (7002): slots 10922-16383 (5,462 slots)

Step 4: Assign replicas to masters (anti-affinity)
  Replica 7003 → follows Master 7000
  Replica 7004 → follows Master 7001
  Replica 7005 → follows Master 7002
```

**Why contiguous ranges?**

Redis uses contiguous ranges for operational simplicity:

✓ **Pros**:
- Easy to visualize: "Master 1 owns slots 0-5460"
- Simple CLUSTER SLOTS response (3 lines instead of 16,384)
- Fast lookups: `if (slot >= 0 && slot <= 5460) → Master 1`
- Efficient slot migration (move ranges, not individual slots)

✗ **Cons**:
- Doesn't matter for hash distribution (CRC16 already randomizes)
- No technical benefit over scattered assignment

**Example with our cluster**:

```bash
# After cluster creation, check slot distribution
redis-cli -p 7000 cluster nodes

# Output shows:
a1b2c3... 127.0.0.1:7000 master - 0 slots:0-5460
d4e5f6... 127.0.0.1:7001 master - 0 slots:5461-10921
g7h8i9... 127.0.0.1:7002 master - 0 slots:10922-16383
```

**Slot assignment during resharding**:

When adding a 4th master node, Redis rebalances slots:

```
Before (3 masters):
Master 1: 5,461 slots
Master 2: 5,461 slots
Master 3: 5,462 slots
Total: 16,384 slots

After adding Master 4 (target distribution):
Master 1: 4,096 slots (removed 1,365)
Master 2: 4,096 slots (removed 1,365)
Master 3: 4,096 slots (removed 1,366)
Master 4: 4,096 slots (received 4,096)
Total: 16,384 slots (conserved)
```

**Rebalancing algorithm**:

```python
def rebalance_slots(masters, total_slots=16384):
    num_masters = len(masters)
    target_per_master = total_slots // num_masters
    remainder = total_slots % num_masters

    # Calculate target distribution
    for i, master in enumerate(masters):
        if i < remainder:
            master.target_slots = target_per_master + 1
        else:
            master.target_slots = target_per_master

    # Move slots from over-allocated to under-allocated
    migrations = []
    for master in masters:
        if master.current_slots > master.target_slots:
            excess = master.current_slots - master.target_slots
            # Find under-allocated masters
            for receiver in masters:
                if receiver.current_slots < receiver.target_slots:
                    slots_to_move = min(excess, receiver.target_slots - receiver.current_slots)
                    migrations.append((master, receiver, slots_to_move))
                    excess -= slots_to_move
                    if excess == 0:
                        break

    return migrations
```

**Example execution**:

```bash
# Before: 3 masters own all slots
redis-cli -p 7000 cluster nodes | grep master
Master 1: slots 0-5460
Master 2: slots 5461-10921
Master 3: slots 10922-16383

# Add new master
redis-cli --cluster add-node 127.0.0.1:7006 127.0.0.1:7000

# Reshard: take 1,365 slots from Master 1, 1,365 from Master 2, 1,366 from Master 3
redis-cli --cluster reshard 127.0.0.1:7000 \
  --cluster-from a1b2c3,d4e5f6,g7h8i9 \  # From all 3 masters
  --cluster-to j1k2l3 \                   # To new master
  --cluster-slots 4096

# After: 4 masters, balanced
redis-cli -p 7000 cluster nodes | grep master
Master 1: slots 0-4095          (4,096 slots)
Master 2: slots 4096-8191       (4,096 slots)
Master 3: slots 8192-12287      (4,096 slots)
Master 4: slots 12288-16383     (4,096 slots)
```

**Slot-to-Node Mapping Data Structure**

Each Redis node maintains a slot map in memory:

```c
// Simplified Redis Cluster internal structure
typedef struct clusterState {
    clusterNode *slots[16384];  // Array mapping slot → node
    // ...
} clusterState;

// Lookup is O(1):
clusterNode* getNodeForSlot(int slot) {
    return server.cluster->slots[slot];
}
```

**Client-side slot caching**:

go-redis (our client library) caches slot mappings:

```go
type ClusterClient struct {
    slotCache map[int]*clusterNode  // slot → node mapping
    mu        sync.RWMutex
}

func (c *ClusterClient) slotForKey(key string) int {
    return int(crc16.Checksum([]byte(key))) % 16384
}

func (c *ClusterClient) cmdSlot(cmd Cmder) int {
    return c.slotForKey(cmd.Key())  // CRC16 hash
}

func (c *ClusterClient) nodeForSlot(slot int) *clusterNode {
    c.mu.RLock()
    node := c.slotCache[slot]
    c.mu.RUnlock()

    if node == nil {
        c.refreshSlotCache()  // Ask cluster for CLUSTER SLOTS
        node = c.slotCache[slot]
    }

    return node
}
```

**Key insight: Deterministic but decentralized**

The slot distribution is:
- ✓ **Deterministic**: CRC16(key) always gives same slot
- ✓ **Decentralized**: Every node knows full slot mapping (no coordinator needed)
- ✓ **Eventually consistent**: Gossip protocol ensures convergence within seconds
- ✗ **Not self-balancing**: Manual reshard needed when adding/removing nodes

**Comparison with other partitioning strategies**:

| Strategy | Used By | Pros | Cons |
|----------|---------|------|------|
| **Hash Slot (16K)** | **Redis Cluster** | **Decentralized, deterministic, easy migration** | **Manual rebalancing** |
| Consistent Hashing | Cassandra, DynamoDB | Auto-rebalancing, minimal data movement | Complex, virtual nodes needed |
| Range Partitioning | HBase, BigTable | Scan-friendly, simple | Hotspots, manual splits |
| Random | Some caches | Simple | No locality, can't migrate |

**Why Redis chose hash slots over consistent hashing**:

```
Consistent Hashing:
✓ Auto-rebalancing when adding nodes
✗ Complex virtual node management
✗ Non-uniform distribution without tuning
✗ Difficult to implement atomic multi-key operations

Hash Slots:
✓ Simple, easy to reason about
✓ Uniform distribution guaranteed
✓ Easy slot migration (move entire slots atomically)
✗ Manual rebalancing (acceptable for Redis's use case)
```

**Summary**:

1. **16,384 slots chosen for**:
   - Optimal gossip protocol size (2KB bitmap)
   - Good hash distribution granularity
   - Supports up to ~16K nodes (far beyond typical usage)

2. **Slot distribution algorithm**:
   - Initial: Round-robin, contiguous ranges for simplicity
   - Reshard: Greedy rebalancing to achieve equal slots per master
   - Migration: Slot-by-slot, gradual, online (no downtime)

3. **Every component knows slot mapping**:
   - Redis nodes: Full cluster state via gossip
   - Clients: Cached slot map, updated on MOVED/ASK redirects
   - Lookups: O(1) array access (deterministic CRC16 hash)

### Q10. Hot Clients and Multi-Tenant Fairness

**The Problem: Noisy Neighbor Effect**

Consider a scenario where one client makes 1 million requests/second while others make 100 req/sec:

```
Client A (1.1.1.1): 1,000,000 req/sec → CRC16 → slot 4231 → Master 1
Client B (2.2.2.2): 100 req/sec       → CRC16 → slot 9876 → Master 2
Client C (3.3.3.3): 100 req/sec       → CRC16 → slot 4500 → Master 1 (same shard!)
```

**What happens?**

```
Master 1 (Overloaded):
├─ Client A: 1M req/sec (99.99% of traffic)
├─ Client C: 100 req/sec
└─ Total: ~1M req/sec → Single-threaded bottleneck!

Master 2 (Idle):
└─ Client B: 100 req/sec → Running at 0.1% capacity

Master 3 (Idle):
└─ No clients → 0% utilization
```

**Problems with our current implementation**:

1. **Hotspot on Master 1**
   - Redis is single-threaded (execution)
   - Master 1 can handle ~100k ops/sec max
   - Client A alone needs 1M ops/sec → **10x overload**
   - Queue builds up, latency increases to seconds
   - Eventually: timeouts, connection exhaustion

2. **Head-of-line blocking for Client C**
   - Client C shares Master 1 with Client A
   - Client C's requests wait behind Client A's massive queue
   - Client C's latency increases from 1ms → 10+ seconds
   - **Unfair**: Client C gets penalized for Client A's behavior

3. **Wasted capacity on Masters 2 & 3**
   - Master 2, Master 3 idle at <1% utilization
   - No load balancing across shards
   - Can't help Master 1 (data locality constraint)

**Current system behavior** (no fairness):

```go
// gateway/ratelimiter/token_bucket.go
func (tb *TokenBucket) Allow(ctx context.Context, key string) (*Result, error) {
    // key = "ratelimit:1.1.1.1"
    // Always routes to same shard (deterministic)
    result, err := tokenBucketScript.Run(ctx, tb.client, []string{key}, ...)
    // If this shard is overloaded, ALL clients on this shard suffer
}
```

**No fairness mechanisms**:
- ❌ No per-client request queuing
- ❌ No priority system
- ❌ No circuit breakers for abusive clients
- ❌ No load shedding
- ❌ No cross-shard load balancing

**Solution 1: Hash Tag Sharding (Split Hot Client)**

Instead of routing all requests from one client to one shard, distribute them:

```go
// Original: All Client A requests → same shard
key := "ratelimit:1.1.1.1"  // → slot 4231 (always)

// Modified: Distribute Client A across multiple shards
func getClientKey(clientIP string, shardCount int) string {
    // Hash the client IP to determine shard ID
    shard := hash(clientIP) % shardCount
    // Use Redis hash tags to control slot assignment
    return fmt.Sprintf("ratelimit:{shard%d}:%s", shard, clientIP)
}

// Example:
getClientKey("1.1.1.1", 10) → "ratelimit:{shard3}:1.1.1.1" → slot X
getClientKey("1.1.1.1", 10) → "ratelimit:{shard3}:1.1.1.1" → slot X (same)

// But with request-level sharding:
func getClientKeyWithRequestSharding(clientIP string, requestID string) string {
    shard := hash(clientIP + requestID) % 10
    return fmt.Sprintf("ratelimit:{shard%d}:%s", shard, clientIP)
}
```

**Implementation**:

```go
// gateway/ratelimiter/sharded_token_bucket.go
type ShardedTokenBucket struct {
    client       redis.Cmdable
    bucketSize   int64
    refillRate   float64
    shardCount   int  // Number of virtual shards per client
}

func (stb *ShardedTokenBucket) Allow(ctx context.Context, clientIP string) (*Result, error) {
    // Distribute client's tokens across multiple shards
    // Each shard gets bucketSize/shardCount tokens

    var totalRemaining int64
    var allowed bool

    // Try shards in round-robin until we find one with tokens
    for i := 0; i < stb.shardCount; i++ {
        shardKey := fmt.Sprintf("ratelimit:{shard%d}:%s", i, clientIP)

        result, err := tokenBucketScript.Run(ctx, stb.client, []string{shardKey},
            stb.bucketSize/int64(stb.shardCount),  // Divide capacity
            stb.refillRate/float64(stb.shardCount), // Divide refill rate
            now,
        ).Int64Slice()

        if err != nil {
            return nil, err
        }

        totalRemaining += result[1]

        if result[0] == 1 {
            allowed = true
            break  // Found a shard with available tokens
        }
    }

    return &Result{
        Allowed:   allowed,
        Remaining: totalRemaining,
        Limit:     stb.bucketSize,
    }, nil
}
```

**Benefits**:
- ✓ Hot client (1M req/sec) spread across 10 shards → 100k req/sec per shard
- ✓ Load distributed across Redis cluster
- ✓ Other clients on same shard not impacted

**Trade-offs**:
- ✗ 10x Redis operations (check 10 shards per request)
- ✗ More complex logic (shard selection)
- ✗ Slight inaccuracy (tokens split across shards, not perfectly atomic)

**Solution 2: Per-Client Connection Limits (Gateway-Level)**

Limit concurrent connections per client at the gateway:

```go
// gateway/main.go
type Gateway struct {
    limiter         *ratelimiter.TokenBucket
    proxy           *httputil.ReverseProxy
    connLimiter     *ConnectionLimiter  // New: per-client connection limit
}

type ConnectionLimiter struct {
    mu              sync.RWMutex
    activeConns     map[string]*semaphore.Weighted  // client IP → semaphore
    maxConnsPerClient int64
}

func NewConnectionLimiter(maxConnsPerClient int64) *ConnectionLimiter {
    return &ConnectionLimiter{
        activeConns:       make(map[string]*semaphore.Weighted),
        maxConnsPerClient: maxConnsPerClient,
    }
}

func (cl *ConnectionLimiter) Acquire(clientIP string, ctx context.Context) error {
    cl.mu.Lock()
    sem, exists := cl.activeConns[clientIP]
    if !exists {
        sem = semaphore.NewWeighted(cl.maxConnsPerClient)
        cl.activeConns[clientIP] = sem
    }
    cl.mu.Unlock()

    // Try to acquire permit (blocks if maxConnsPerClient reached)
    return sem.Acquire(ctx, 1)
}

func (cl *ConnectionLimiter) Release(clientIP string) {
    cl.mu.RLock()
    sem := cl.activeConns[clientIP]
    cl.mu.RUnlock()

    if sem != nil {
        sem.Release(1)
    }
}

// Updated handler
func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
    clientIP := getClientIP(r)

    // Limit concurrent connections per client
    ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
    defer cancel()

    if err := g.connLimiter.Acquire(clientIP, ctx); err != nil {
        // Connection limit exceeded or timeout
        http.Error(w, "Too many concurrent requests", http.StatusTooManyRequests)
        return
    }
    defer g.connLimiter.Release(clientIP)

    // Proceed with rate limiting...
    result, err := g.limiter.Allow(r.Context(), "ratelimit:"+clientIP)
    // ... rest of handler
}
```

**Benefits**:
- ✓ Prevents single client from monopolizing gateway resources
- ✓ Fast (in-memory, no Redis call)
- ✓ Protects against connection exhaustion

**Trade-offs**:
- ✗ Only works within one gateway instance (not cluster-wide)
- ✗ Doesn't protect Redis (only gateway)

**Solution 3: Adaptive Rate Limiting (Penalize Abusers)**

Dynamically lower rate limits for clients exhibiting abusive behavior:

```go
// gateway/ratelimiter/adaptive_limiter.go
type AdaptiveTokenBucket struct {
    client           redis.Cmdable
    baseBucketSize   int64
    baseRefillRate   float64
    penaltyTracker   *PenaltyTracker
}

type PenaltyTracker struct {
    mu            sync.RWMutex
    violations    map[string]int     // client IP → violation count
    lastReset     map[string]time.Time
    resetInterval time.Duration
}

func (at *AdaptiveTokenBucket) Allow(ctx context.Context, clientIP string) (*Result, error) {
    // Check violation history
    violations := at.penaltyTracker.GetViolations(clientIP)

    // Calculate penalty multiplier (exponential backoff)
    // 0 violations: 1x limit
    // 1 violation:  0.5x limit
    // 2 violations: 0.25x limit
    // 3+ violations: 0.1x limit
    penaltyMultiplier := 1.0 / math.Pow(2, float64(violations))
    if penaltyMultiplier < 0.1 {
        penaltyMultiplier = 0.1  // Minimum 10% of base limit
    }

    adjustedBucketSize := int64(float64(at.baseBucketSize) * penaltyMultiplier)
    adjustedRefillRate := at.baseRefillRate * penaltyMultiplier

    key := "ratelimit:" + clientIP
    result, err := tokenBucketScript.Run(ctx, at.client, []string{key},
        adjustedBucketSize,
        adjustedRefillRate,
        now,
    ).Int64Slice()

    if err != nil {
        return nil, err
    }

    // Track violations (rate limit exceeded)
    if result[0] == 0 {
        at.penaltyTracker.RecordViolation(clientIP)
    }

    return &Result{
        Allowed:   result[0] == 1,
        Remaining: result[1],
        Limit:     adjustedBucketSize,
    }, nil
}

func (pt *PenaltyTracker) RecordViolation(clientIP string) {
    pt.mu.Lock()
    defer pt.mu.Unlock()

    // Reset violations if interval expired
    if lastReset, exists := pt.lastReset[clientIP]; exists {
        if time.Since(lastReset) > pt.resetInterval {
            pt.violations[clientIP] = 0
            pt.lastReset[clientIP] = time.Now()
        }
    } else {
        pt.lastReset[clientIP] = time.Now()
    }

    pt.violations[clientIP]++
}
```

**Benefits**:
- ✓ Self-healing (penalized clients get restored after good behavior)
- ✓ Protects well-behaved clients
- ✓ No infrastructure changes needed

**Trade-offs**:
- ✗ Reactive (must observe violations first)
- ✗ Gateway-local tracking (not distributed)
- ✗ Complexity in violation tracking

**Solution 4: Separate Redis Pools (Tenant Isolation)**

For true multi-tenancy, isolate high-value clients:

```go
// gateway/main.go
type TieredGateway struct {
    premiumLimiter  *ratelimiter.TokenBucket  // Redis Cluster A (dedicated)
    standardLimiter *ratelimiter.TokenBucket  // Redis Cluster B (shared)
    tierClassifier  *TierClassifier
}

type TierClassifier struct {
    premiumClients map[string]bool  // IP → tier mapping
}

func (tg *TieredGateway) handleRequest(w http.ResponseWriter, r *http.Request) {
    clientIP := getClientIP(r)

    // Route to appropriate limiter based on tier
    var limiter *ratelimiter.TokenBucket
    if tg.tierClassifier.IsPremium(clientIP) {
        limiter = tg.premiumLimiter  // Dedicated Redis cluster
    } else {
        limiter = tg.standardLimiter // Shared Redis cluster
    }

    result, err := limiter.Allow(r.Context(), "ratelimit:"+clientIP)
    // ... rest of handler
}
```

**Infrastructure**:

```
Premium Redis Cluster (isolated):
├─ Master 1 (premium clients only)
├─ Master 2 (premium clients only)
└─ Master 3 (premium clients only)

Standard Redis Cluster (shared):
├─ Master 4 (all other clients)
├─ Master 5 (all other clients)
└─ Master 6 (all other clients)
```

**Benefits**:
- ✓ Complete isolation (noisy neighbors can't impact premium clients)
- ✓ Different SLAs per tier
- ✓ Scale independently (add capacity to only one tier)

**Trade-offs**:
- ✗ High cost (2x Redis infrastructure)
- ✗ Operational complexity (manage 2 clusters)
- ✗ Resource underutilization (premium cluster may be idle)

**Solution 5: Load Shedding with Circuit Breakers**

Automatically reject requests when a shard is overloaded:

```go
// gateway/ratelimiter/circuit_breaker.go
type CircuitBreakerTokenBucket struct {
    client          redis.Cmdable
    bucketSize      int64
    refillRate      float64
    breakers        map[string]*CircuitBreaker  // shard → breaker
    latencyTracker  *LatencyTracker
}

type CircuitBreaker struct {
    state           int32  // 0=closed, 1=open, 2=half-open
    failureCount    int32
    lastFailureTime time.Time
    threshold       int
    timeout         time.Duration
}

func (cb *CircuitBreakerTokenBucket) Allow(ctx context.Context, clientIP string) (*Result, error) {
    key := "ratelimit:" + clientIP
    shard := cb.getShardForKey(key)

    breaker := cb.breakers[shard]

    // Check circuit breaker state
    if breaker.IsOpen() {
        // Shard is overloaded, fail fast
        return &Result{
            Allowed:    false,
            Remaining:  0,
            Limit:      cb.bucketSize,
            RetryAfter: breaker.timeout,
        }, nil
    }

    // Measure latency
    start := time.Now()
    result, err := tokenBucketScript.Run(ctx, cb.client, []string{key},
        cb.bucketSize,
        cb.refillRate,
        float64(time.Now().UnixNano())/float64(time.Second),
    ).Int64Slice()
    latency := time.Since(start)

    // Trip breaker if latency exceeds threshold
    if latency > 100*time.Millisecond {
        breaker.RecordFailure()
        if breaker.failureCount > int32(breaker.threshold) {
            breaker.Open()
            log.Printf("Circuit breaker OPEN for shard %s (latency: %v)", shard, latency)
        }
    } else {
        breaker.RecordSuccess()
    }

    if err != nil {
        return nil, err
    }

    return &Result{
        Allowed:   result[0] == 1,
        Remaining: result[1],
        Limit:     cb.bucketSize,
    }, nil
}
```

**Benefits**:
- ✓ Protects Redis from overload (fail fast)
- ✓ Prevents cascading failures
- ✓ Self-healing (circuit closes after timeout)

**Trade-offs**:
- ✗ Legitimate requests may be rejected (false positives)
- ✗ Tuning threshold is tricky (too sensitive → unnecessary rejections)

**Comparison of Solutions**:

| Solution | Fairness | Complexity | Cost | Latency Impact |
|----------|----------|------------|------|----------------|
| **Hash Tag Sharding** | ✓✓ Good | Medium | None | +10% (multi-shard checks) |
| **Connection Limits** | ✓ Fair | Low | None | None |
| **Adaptive Limiting** | ✓✓ Good | Medium | None | None |
| **Separate Pools** | ✓✓✓ Excellent | High | High (2x infra) | None |
| **Circuit Breakers** | ✓ Fair | Medium | None | None (fail fast) |

**Production Recommendation: Layered Defense**

Combine multiple approaches for robust multi-tenant fairness:

```go
// gateway/main.go - Production setup
type ProductionGateway struct {
    // Layer 1: Connection limiting (fast, in-memory)
    connLimiter *ConnectionLimiter

    // Layer 2: Adaptive rate limiting (Redis-based, tracks history)
    adaptiveLimiter *AdaptiveTokenBucket

    // Layer 3: Circuit breaker (protects against shard overload)
    breakerLimiter *CircuitBreakerTokenBucket

    // Layer 4: Tier isolation (premium vs. standard)
    premiumLimiter *ratelimiter.TokenBucket
    standardLimiter *ratelimiter.TokenBucket

    tierClassifier *TierClassifier
}

func (pg *ProductionGateway) handleRequest(w http.ResponseWriter, r *http.Request) {
    clientIP := getClientIP(r)

    // Layer 1: Check connection limit (fail fast if too many concurrent)
    if err := pg.connLimiter.Acquire(clientIP, r.Context()); err != nil {
        http.Error(w, "Too many concurrent requests", 429)
        return
    }
    defer pg.connLimiter.Release(clientIP)

    // Layer 2: Determine tier and select appropriate limiter
    var limiter *AdaptiveTokenBucket
    if pg.tierClassifier.IsPremium(clientIP) {
        limiter = pg.premiumLimiter  // Isolated Redis cluster
    } else {
        limiter = pg.standardLimiter // Shared Redis cluster
    }

    // Layer 3: Apply adaptive rate limiting (with circuit breaker)
    result, err := limiter.Allow(r.Context(), clientIP)
    if err != nil {
        log.Printf("Rate limiter error: %v", err)
        // Fail open (or closed, depending on policy)
        w.Header().Set("X-RateLimit-Warning", "rate-limiter-error")
        pg.proxy.ServeHTTP(w, r)
        return
    }

    // Set rate limit headers
    w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
    w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))

    if !result.Allowed {
        w.Header().Set("X-RateLimit-Retry-After", strconv.FormatInt(int64(result.RetryAfter.Seconds()), 10))
        http.Error(w, "Rate limit exceeded", 429)
        return
    }

    // Forward to backend
    pg.proxy.ServeHTTP(w, r)
}
```

**Monitoring and Alerting**:

```go
// Track per-client metrics
type MetricsCollector struct {
    requestsPerClient  *prometheus.CounterVec   // client_ip, status
    latencyPerShard    *prometheus.HistogramVec // shard_id
    violationsPerClient *prometheus.CounterVec  // client_ip
}

func (mc *MetricsCollector) RecordRequest(clientIP, shard string, latency time.Duration, allowed bool) {
    status := "allowed"
    if !allowed {
        status = "rejected"
    }

    mc.requestsPerClient.WithLabelValues(clientIP, status).Inc()
    mc.latencyPerShard.WithLabelValues(shard).Observe(latency.Seconds())

    if !allowed {
        mc.violationsPerClient.WithLabelValues(clientIP).Inc()
    }
}

// Alert when:
// - Single client exceeds 10% of total traffic
// - Shard latency > 100ms (p99)
// - Client violation rate > 50%
```

**Summary**:

**The hotspot problem is real and serious**:
- Single hot client can monopolize one Redis shard
- Head-of-line blocking impacts all clients on that shard
- No fairness in basic hash slot implementation

**Solutions (in order of recommendation)**:
1. **Connection limits** (easy, effective for gateway protection)
2. **Adaptive rate limiting** (penalize abusers automatically)
3. **Circuit breakers** (protect Redis from overload)
4. **Hash tag sharding** (distribute hot clients across shards)
5. **Tier isolation** (premium clients get dedicated infrastructure)

**For production multi-tenant systems**:
- Implement layers 1-3 at minimum (defense in depth)
- Monitor per-client metrics (detect hot clients early)
- Add tier isolation for high-value customers
- Use autoscaling to add Redis capacity when needed

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
