# Redis Developer Guide

This guide covers Redis internals, cluster configuration, Lua scripting, and debugging techniques for the rate limiter system.

## Table of Contents

1. [Redis Architecture Overview](#redis-architecture-overview)
2. [Standalone vs Cluster Mode](#standalone-vs-cluster-mode)
3. [Lua Scripting in Redis](#lua-scripting-in-redis)
4. [Cluster Configuration Deep Dive](#cluster-configuration-deep-dive)
5. [Client Configuration (go-redis)](#client-configuration-go-redis)
6. [Debugging the Cluster](#debugging-the-cluster)
7. [Performance Tuning](#performance-tuning)
8. [Failure Scenarios and Recovery](#failure-scenarios-and-recovery)
9. [Production Considerations](#production-considerations)

---

## Redis Architecture Overview

### How Redis Stores Data

Redis is an in-memory data store. All data lives in RAM, making it extremely fast (sub-millisecond latency). Data is persisted to disk asynchronously.

```
┌─────────────────────────────────────────────────────────────────┐
│                         REDIS SERVER                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐        │
│   │   Hash      │    │   String    │    │   List      │        │
│   │             │    │             │    │             │        │
│   │ ratelimit:  │    │ session:    │    │ queue:      │        │
│   │ 192.168.1.1 │    │ abc123      │    │ jobs        │        │
│   │             │    │             │    │             │        │
│   │ tokens: 7.5 │    │ "user_data" │    │ [job1,job2] │        │
│   │ last: 17283 │    │             │    │             │        │
│   └─────────────┘    └─────────────┘    └─────────────┘        │
│                                                                 │
│   RAM (Primary Storage)                                         │
│                                                                 │
├─────────────────────────────────────────────────────────────────┤
│   Disk (Persistence: RDB snapshots + AOF log)                   │
└─────────────────────────────────────────────────────────────────┘
```

### Our Data Model

The rate limiter uses Redis Hashes to store token bucket state:

```
Key:    ratelimit:{client_ip}
Type:   Hash
Fields:
  - tokens:     Current token count (float)
  - last_refill: Timestamp of last refill (Unix seconds)
TTL:    3600 seconds (1 hour of inactivity)
```

Example:
```bash
$ redis-cli HGETALL ratelimit:192.168.1.1
1) "tokens"
2) "7.5"
3) "last_refill"
4) "1704307200.123"
```

---

## Standalone vs Cluster Mode

### Standalone Mode

Single Redis instance. Simple but no high availability.

```
┌──────────┐     ┌─────────────────┐
│  Client  │────▶│  Redis :6379    │
└──────────┘     └─────────────────┘
```

**Use when:**
- Development/testing
- Low traffic (< 100k ops/sec)
- Downtime is acceptable

**Configuration:**
```bash
REDIS_MODE=standalone REDIS_ADDR=localhost:6379 ./run.sh
```

### Cluster Mode

Data is sharded across multiple masters. Each master has replicas for failover.

```
┌──────────┐     ┌─────────────────────────────────────────────┐
│  Client  │────▶│              Redis Cluster                  │
└──────────┘     │                                             │
                 │  ┌─────────┐  ┌─────────┐  ┌─────────┐     │
                 │  │Master 0 │  │Master 1 │  │Master 2 │     │
                 │  │:7000    │  │:7001    │  │:7002    │     │
                 │  │slots    │  │slots    │  │slots    │     │
                 │  │0-5460   │  │5461-    │  │10923-   │     │
                 │  │         │  │10922    │  │16383    │     │
                 │  └────┬────┘  └────┬────┘  └────┬────┘     │
                 │       │            │            │           │
                 │  ┌────▼────┐  ┌────▼────┐  ┌────▼────┐     │
                 │  │Replica  │  │Replica  │  │Replica  │     │
                 │  │:7003    │  │:7004    │  │:7005    │     │
                 │  └─────────┘  └─────────┘  └─────────┘     │
                 └─────────────────────────────────────────────┘
```

**Key Concepts:**

| Concept | Description |
|---------|-------------|
| **Slots** | Redis divides keyspace into 16384 slots (0-16383) |
| **Sharding** | Each master owns a range of slots |
| **Key → Slot** | `slot = CRC16(key) % 16384` |
| **Replication** | Each master has 1+ replicas for failover |
| **Failover** | Replica auto-promotes when master dies |

**Configuration:**
```bash
REDIS_MODE=cluster REDIS_ADDRS=localhost:7000,localhost:7001,localhost:7002 ./run.sh
```

### How Keys Are Routed

When a client wants to access `ratelimit:192.168.1.1`:

```
1. Calculate slot:  CRC16("ratelimit:192.168.1.1") % 16384 = 12345
2. Find master:     Slot 12345 is in range 10923-16383 → Master :7002
3. Execute:         Command runs on :7002
```

If the client connects to the wrong node, Redis returns a MOVED redirect:
```
$ redis-cli -p 7000 GET ratelimit:192.168.1.1
(error) MOVED 12345 127.0.0.1:7002
```

Smart clients (like go-redis) handle this automatically.

---

## Lua Scripting in Redis

### Why Lua?

Redis executes Lua scripts **atomically**. The entire script runs without interruption, solving race conditions.

**The Problem (without Lua):**
```
Time    Client A                    Client B
─────────────────────────────────────────────────
T1      GET tokens → 5
T2                                  GET tokens → 5
T3      SET tokens = 4
T4                                  SET tokens = 4  ← WRONG! Should be 3
```

Both clients read 5, both write 4. We lost a token.

**The Solution (with Lua):**
```
Time    Client A                    Client B
─────────────────────────────────────────────────
T1      EVAL script → 4             (blocked)
T2                                  EVAL script → 3
```

Lua script executes atomically. No race condition.

### How Lua Runs in Redis

```
┌─────────────────────────────────────────────────────────────────┐
│                      REDIS SERVER                               │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   ┌───────────────────────────────────────────────────────┐    │
│   │                   LUA INTERPRETER                      │    │
│   │                                                        │    │
│   │   1. Script received via EVAL/EVALSHA                  │    │
│   │   2. Redis BLOCKS all other commands                   │    │
│   │   3. Script executes with redis.call()                 │    │
│   │   4. Result returned, Redis UNBLOCKS                   │    │
│   │                                                        │    │
│   │   ┌────────────────────────────────────────────────┐  │    │
│   │   │  local tokens = redis.call('HGET', key, 'tok') │  │    │
│   │   │  tokens = tokens - 1                           │  │    │
│   │   │  redis.call('HSET', key, 'tokens', tokens)     │  │    │
│   │   │  return tokens                                 │  │    │
│   │   └────────────────────────────────────────────────┘  │    │
│   │                                                        │    │
│   └───────────────────────────────────────────────────────┘    │
│                                                                 │
│   Data Store (blocked during script execution)                  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Script Caching with EVALSHA

Loading the full script every time is wasteful. Redis caches scripts by SHA1 hash:

```
First call:   EVAL "local tokens = ..." 1 key    → Slow (sends full script)
              Redis caches script, returns SHA1: "a1b2c3d4..."

Next calls:   EVALSHA "a1b2c3d4..." 1 key        → Fast (just the hash)
```

Our go-redis client handles this automatically with `script.Run()`.

### Our Token Bucket Lua Script

```lua
-- token_bucket.lua (embedded in token_bucket.go)

-- KEYS[1] = rate limit key (e.g., "ratelimit:192.168.1.1")
-- ARGV[1] = bucket_size (max tokens)
-- ARGV[2] = refill_rate (tokens per second)
-- ARGV[3] = current timestamp

local key = KEYS[1]
local bucket_size = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

-- Get current state (or initialize)
local tokens = tonumber(redis.call('HGET', key, 'tokens'))
local last_refill = tonumber(redis.call('HGET', key, 'last_refill'))

if tokens == nil then
    -- First request from this client
    tokens = bucket_size
    last_refill = now
end

-- Refill tokens based on elapsed time
local elapsed = now - last_refill
local refill = elapsed * refill_rate
tokens = math.min(bucket_size, tokens + refill)

-- Try to consume a token
local allowed = 0
local retry_after = 0

if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
else
    -- Calculate wait time for next token
    retry_after = (1 - tokens) / refill_rate
end

-- Save state
redis.call('HSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('EXPIRE', key, 3600)  -- Expire after 1 hour of inactivity

return {allowed, tokens, retry_after}
```

### Lua Script Constraints

| Constraint | Reason |
|------------|--------|
| **No external I/O** | Scripts can't access network, files, etc. |
| **Deterministic** | Same inputs must produce same outputs (for replication) |
| **Time limits** | Default 5 second limit (configurable with `lua-time-limit`) |
| **Memory limits** | Controlled by `maxmemory` setting |
| **Single-threaded** | Blocks all other operations while running |

### Debugging Lua Scripts

Test scripts directly with redis-cli:
```bash
# Test the token bucket script
redis-cli EVAL "
local tokens = redis.call('HGET', KEYS[1], 'tokens')
return tokens or 'nil'
" 1 ratelimit:test-client

# Watch script execution
redis-cli MONITOR  # Shows all commands including Lua internals
```

---

## Cluster Configuration Deep Dive

### Creating a Cluster

Our `cluster-setup.sh` script does this:

```bash
# 1. Start 6 Redis instances (3 masters + 3 replicas)
for port in 7000 7001 7002 7003 7004 7005; do
    redis-server --port $port --cluster-enabled yes --cluster-config-file nodes-$port.conf
done

# 2. Create the cluster
redis-cli --cluster create \
    127.0.0.1:7000 127.0.0.1:7001 127.0.0.1:7002 \
    127.0.0.1:7003 127.0.0.1:7004 127.0.0.1:7005 \
    --cluster-replicas 1
```

### Key Configuration Options

**redis.conf for cluster nodes:**
```ini
# Cluster mode
cluster-enabled yes
cluster-config-file nodes-7000.conf
cluster-node-timeout 5000          # 5 seconds to detect failure

# Replication
replica-read-only yes              # Replicas don't accept writes
replica-serve-stale-data yes       # Serve data during sync

# Persistence
appendonly yes                     # Enable AOF for durability
appendfsync everysec               # Sync to disk every second

# Memory
maxmemory 256mb                    # Limit memory usage
maxmemory-policy allkeys-lru       # Evict least recently used keys
```

### Slot Management

```bash
# View slot distribution
redis-cli -p 7000 cluster slots

# Reshard slots between nodes
redis-cli --cluster reshard 127.0.0.1:7000

# Rebalance cluster
redis-cli --cluster rebalance 127.0.0.1:7000
```

---

## Client Configuration (go-redis)

### ClusterClient Options

```go
redis.NewClusterClient(&redis.ClusterOptions{
    Addrs:          []string{"localhost:7000", "localhost:7001", "localhost:7002"},

    // Timeouts
    DialTimeout:    2 * time.Second,   // Connection timeout
    ReadTimeout:    1 * time.Second,   // Read timeout
    WriteTimeout:   1 * time.Second,   // Write timeout

    // Routing
    ReadOnly:       true,              // Allow reads from replicas
    RouteRandomly:  true,              // Distribute reads across replicas
    RouteByLatency: false,             // Route to lowest latency node

    // Reliability
    MaxRetries:     3,                 // Retry failed commands
    MinRetryBackoff: 8 * time.Millisecond,
    MaxRetryBackoff: 512 * time.Millisecond,

    // Connection pool
    PoolSize:       10,                // Connections per node
    MinIdleConns:   5,                 // Keep connections warm
    PoolTimeout:    4 * time.Second,   // Wait for connection
})
```

### Read/Write Routing Behavior

| Setting | Reads | Writes | Use Case |
|---------|-------|--------|----------|
| Default | Master | Master | Strong consistency |
| `ReadOnly: true` | Replica | Master | Scale reads, eventual consistency |
| `RouteRandomly: true` | Random replica | Master | Distribute load |
| `RouteByLatency: true` | Nearest node | Master | Minimize latency |

### Handling Cluster Errors

```go
result, err := client.Get(ctx, key).Result()
if err != nil {
    switch {
    case errors.Is(err, redis.Nil):
        // Key doesn't exist
    case errors.Is(err, context.DeadlineExceeded):
        // Timeout
    case strings.Contains(err.Error(), "MOVED"):
        // Cluster resharding, client should retry
    case strings.Contains(err.Error(), "CLUSTERDOWN"):
        // Cluster is unhealthy
    default:
        // Network error, connection refused, etc.
    }
}
```

---

## Debugging the Cluster

### Essential Commands

**Cluster Health:**
```bash
# Overall cluster state
redis-cli -p 7000 cluster info
# Look for: cluster_state:ok, cluster_slots_ok:16384

# Node status
redis-cli -p 7000 cluster nodes
# Shows all nodes, roles, slots, and connection status

# Check for failures
redis-cli -p 7000 cluster nodes | grep fail
```

**Key Inspection:**
```bash
# Find which slot a key belongs to
redis-cli -p 7000 cluster keyslot "ratelimit:192.168.1.1"
# Returns: 12345

# Find which node owns a slot
redis-cli -p 7000 cluster slots
# Shows slot ranges and their masters/replicas

# Get key data
redis-cli -c -p 7000 HGETALL ratelimit:192.168.1.1
# -c flag enables cluster mode (follows redirects)
```

**Live Monitoring:**
```bash
# Watch all commands in real-time
redis-cli -p 7000 MONITOR

# Watch specific patterns
redis-cli -p 7000 MONITOR | grep ratelimit

# Slow log (commands taking > 10ms)
redis-cli -p 7000 SLOWLOG GET 10
```

**Memory Analysis:**
```bash
# Memory usage for a key
redis-cli -p 7000 MEMORY USAGE ratelimit:192.168.1.1

# Overall memory stats
redis-cli -p 7000 INFO memory

# Find big keys
redis-cli -p 7000 --bigkeys
```

### Common Issues and Solutions

**1. CLUSTERDOWN - Cluster is down**
```bash
# Check which slots are missing
redis-cli -p 7000 cluster info | grep cluster_slots

# Check for failed nodes
redis-cli -p 7000 cluster nodes | grep fail

# Fix: Restart failed nodes or reshard
```

**2. MOVED errors in logs**
```
MOVED 12345 127.0.0.1:7002
```
This is normal during resharding. Smart clients handle it automatically.

**3. Connection refused to specific node**
```bash
# Check if node is running
redis-cli -p 7002 ping

# Check what's on that port
lsof -i:7002

# Restart the node
cd /tmp/redis-cluster/7002 && redis-server redis.conf
```

**4. Replica not syncing**
```bash
# Check replication status
redis-cli -p 7003 INFO replication
# Look for: master_link_status:up

# Force re-sync
redis-cli -p 7003 CLUSTER REPLICATE <master-node-id>
```

### Debug Script for Quick Health Check

```bash
#!/bin/bash
# cluster-health.sh

echo "=== Cluster State ==="
redis-cli -p 7000 cluster info | grep -E "cluster_state|cluster_slots|cluster_known_nodes"

echo ""
echo "=== Node Status ==="
redis-cli -p 7000 cluster nodes | awk '{
    split($2, addr, ":");
    port = addr[2];
    gsub(/@.*/, "", port);
    if ($3 ~ /master/) printf "  MASTER :%s %s\n", port, $9;
    else if ($3 ~ /slave/) printf "  REPLICA :%s → %s\n", port, substr($4,1,8);
    if ($3 ~ /fail/) printf "  ^^^ FAILED ^^^\n";
}'

echo ""
echo "=== Memory Usage ==="
for port in 7000 7001 7002; do
    mem=$(redis-cli -p $port INFO memory | grep used_memory_human | cut -d: -f2 | tr -d '\r')
    echo "  :$port → $mem"
done
```

---

## Performance Tuning

### Latency Optimization

**1. Connection Pooling**
```go
PoolSize:     10,              // Connections per node
MinIdleConns: 5,               // Pre-warm connections
```

**2. Pipelining**
```go
// Bad: 3 round trips
client.Get(ctx, "key1")
client.Get(ctx, "key2")
client.Get(ctx, "key3")

// Good: 1 round trip
pipe := client.Pipeline()
pipe.Get(ctx, "key1")
pipe.Get(ctx, "key2")
pipe.Get(ctx, "key3")
pipe.Exec(ctx)
```

**3. Lua Script Caching**
Scripts are automatically cached by SHA1 hash. First call is slow, subsequent calls use EVALSHA.

### Throughput Optimization

**1. Cluster Sharding**
More masters = more throughput. Each master handles its slot range independently.

```
3 masters:  ~300k ops/sec total
6 masters:  ~600k ops/sec total
```

**2. Read Replicas**
Enable `ReadOnly: true` to scale reads across replicas.

**3. Key Distribution**
Ensure keys are evenly distributed across slots. Avoid hotspots.

```bash
# Check key distribution
for port in 7000 7001 7002; do
    count=$(redis-cli -p $port DBSIZE | awk '{print $2}')
    echo "  :$port → $count keys"
done
```

### Memory Optimization

**1. Set Limits**
```ini
maxmemory 256mb
maxmemory-policy allkeys-lru
```

**2. TTL on Keys**
```lua
redis.call('EXPIRE', key, 3600)  -- Expire inactive keys
```

**3. Efficient Data Structures**
Hash is more memory-efficient than separate keys:
```bash
# Bad: 2 keys
SET ratelimit:192.168.1.1:tokens 7.5
SET ratelimit:192.168.1.1:last_refill 1704307200

# Good: 1 hash
HSET ratelimit:192.168.1.1 tokens 7.5 last_refill 1704307200
```

---

## Failure Scenarios and Recovery

### Scenario 1: Master Node Failure

**Timeline:**
```
T+0s:     Master :7002 crashes
T+0-5s:   Cluster detects failure (cluster-node-timeout)
T+5s:     Replicas vote, one is promoted to master
T+5s+:    Cluster state returns to "ok"
```

**What happens to requests:**
- Writes to affected slots fail until failover completes
- Our gateway: fails open, allows requests without rate limiting
- After failover: normal operation resumes

**Recovery:**
```bash
# Restart failed node - it rejoins as replica
cd /tmp/redis-cluster/7002 && redis-server redis.conf
```

### Scenario 2: Network Partition

**Situation:** Master can't reach other masters but replica can.

```
┌─────────┐         ┌─────────┐
│ Master  │ ──X──── │ Master  │
│  :7000  │         │  :7001  │
└────┬────┘         └─────────┘
     │ (connected)
┌────▼────┐
│ Replica │
│  :7003  │
└─────────┘
```

**Behavior:** Replica may be promoted. When partition heals, old master becomes replica.

### Scenario 3: All Replicas for a Master Fail

**Situation:** Master :7002 and its replica :7005 both fail.

**Behavior:** Cluster goes to "fail" state. Slots 10923-16383 are unavailable.

**Recovery:**
```bash
# Option 1: Restart at least one node
redis-server /tmp/redis-cluster/7002/redis.conf

# Option 2: Reshard slots to remaining masters
redis-cli --cluster reshard 127.0.0.1:7000
```

---

## Production Considerations

### Deployment Checklist

- [ ] **Persistence:** Enable AOF with `appendfsync everysec`
- [ ] **Memory limits:** Set `maxmemory` with appropriate eviction policy
- [ ] **Monitoring:** Track cluster state, memory, latency, errors
- [ ] **Backups:** Regular RDB snapshots to external storage
- [ ] **Security:** Enable AUTH, use TLS in transit
- [ ] **Replicas:** At least 1 replica per master
- [ ] **Anti-affinity:** Place replicas on different hosts than their masters

### Monitoring Metrics

| Metric | Alert Threshold | Command |
|--------|-----------------|---------|
| Cluster state | != "ok" | `cluster info \| grep cluster_state` |
| Memory usage | > 80% | `info memory \| grep used_memory_rss` |
| Connected clients | > pool size | `info clients \| grep connected_clients` |
| Rejected connections | > 0 | `info stats \| grep rejected_connections` |
| Keyspace misses | High ratio | `info stats \| grep keyspace_misses` |

### Scaling Strategies

**Vertical Scaling:**
- More RAM → More data
- Faster CPU → Faster Lua execution
- Faster network → Lower latency

**Horizontal Scaling:**
- More masters → More write throughput
- More replicas → More read throughput
- More clusters (by region) → Geographic distribution

### Multi-Region Considerations

For global rate limiting, options include:

1. **Single Region:** All traffic routes to one cluster. Simple but adds latency.

2. **Regional Clusters:** Each region has its own cluster. Fast but limits are per-region.

3. **CRDTs:** Conflict-free replicated data types. Eventually consistent global limits.

4. **Hybrid:** Local cluster for speed, async sync for global awareness.

---

## Quick Reference

### Common Commands

```bash
# Cluster
redis-cli -p 7000 cluster info
redis-cli -p 7000 cluster nodes
redis-cli -p 7000 cluster slots

# Keys
redis-cli -c -p 7000 HGETALL ratelimit:client
redis-cli -p 7000 cluster keyslot "ratelimit:client"

# Debugging
redis-cli -p 7000 MONITOR
redis-cli -p 7000 SLOWLOG GET 10
redis-cli -p 7000 INFO all

# Management
redis-cli --cluster check 127.0.0.1:7000
redis-cli --cluster reshard 127.0.0.1:7000
redis-cli --cluster rebalance 127.0.0.1:7000
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_MODE` | standalone | `standalone` or `cluster` |
| `REDIS_ADDR` | localhost:6379 | Standalone Redis address |
| `REDIS_ADDRS` | localhost:7000,... | Cluster node addresses |
| `BUCKET_SIZE` | 10 | Max tokens per client |
| `REFILL_RATE` | 1.0 | Tokens added per second |

### File Locations

```
/tmp/redis-cluster/
├── 7000/
│   ├── redis.conf           # Node configuration
│   ├── nodes-7000.conf      # Cluster state (auto-managed)
│   ├── appendonly.aof       # Append-only file
│   └── dump.rdb             # RDB snapshot
├── 7001/
│   └── ...
└── ...
```
