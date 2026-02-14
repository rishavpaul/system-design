# Raft Consensus Algorithm

## 1. What is Raft?

Raft is a consensus algorithm that enables a cluster of servers to agree on a shared state even when some servers fail. It allows multiple independent servers to maintain identical log replicas and execute commands in the same order, providing fault tolerance and data consistency.

### What Raft Enables (TL;DR)

| Capability | Details |
|------------|---------|
| **Strongly consistent writes** | If acknowledged, data is never lost |
| **Minority failure tolerance** | Survives (N-1)/2 failures (1 of 3, 2 of 5, 3 of 7) |
| **Automatic failover** | No human intervention needed |
| **Split-brain prevention** | Impossible to have two leaders |

| Limitation | Details |
|------------|---------|
| **Small clusters only** | 3-7 nodes (not 100s) |
| **Limited throughput** | ~10K writes/sec (single leader bottleneck) |
| **Not for large datasets** | Best for metadata, config, coordination |

**One-liner:** Raft gives you bulletproof consistency for small, critical data — but you can't scale it horizontally.

### Why Raft Exists

Before Raft (2014), **Paxos** was the standard consensus algorithm. But Paxos was notoriously difficult to understand and implement correctly. Raft was designed to be **understandable** while providing the same guarantees.

**Core guarantee:** If a write is acknowledged, it will **never be lost** even if servers crash.

### Why Raft Can't Scale for High-Write Databases

Raft has fundamental architectural bottlenecks that make it unsuitable for high-throughput data systems:

#### 1. Single Leader Bottleneck

```
ALL writes must go through ONE leader

Client A ──┐
Client B ──┼──► Leader ──► Followers
Client C ──┘

Throughput ceiling = What 1 machine can handle
```

**Problem:** Leader's CPU, memory, and network become the bottleneck. You can't add more nodes to increase write throughput — more followers just means more replication work for the leader.

#### 2. Synchronous Replication Latency

```
Write path:
1. Client sends write to Leader         [1ms network]
2. Leader writes to local log           [1ms disk]
3. Leader sends to ALL followers        [1ms network]
4. Wait for MAJORITY to acknowledge     [1ms network + disk]
5. Leader commits and responds          [1ms network]

Total: ~5-10ms per write (best case)
```

**Problem:** Every write waits for network round-trips. At 10ms per write, max throughput ≈ 100 writes/sec per client. Batching helps, but latency remains.

#### 3. Cluster Size Limits

| Nodes | Fault Tolerance | Latency Impact |
|-------|-----------------|----------------|
| 3     | 1 failure       | Baseline |
| 5     | 2 failures      | +20% latency |
| 7     | 3 failures      | +40% latency |
| 9+    | Diminishing returns | Significant overhead |

**Problem:** More nodes = more replication = higher latency. You can't scale horizontally.

#### 4. Log Replication is Sequential

```
Log: [Entry 1] [Entry 2] [Entry 3] [Entry 4] ...
              ▲
              └── Must be applied IN ORDER

No parallel writes. No sharding within a Raft group.
```

**Problem:** Even if you have 1000 independent keys, writes are serialized through one log.

### Comparison: Raft vs High-Scale Alternatives

| Aspect | Raft | Sharded DB (Cassandra) | Primary-Backup (Redis) |
|--------|------|------------------------|------------------------|
| **Write throughput** | ~10K/sec | **Millions/sec** | **100K+/sec** |
| **Latency** | 5-10ms | 1-5ms | **<1ms** |
| **Horizontal scaling** | No | **Yes (add shards)** | Limited |
| **Consistency** | **Strong** | Eventual | Eventual |
| **Data loss on failure** | **None** | Possible | Possible |
| **Split-brain safe** | **Yes** | No | No |

### When to Use What

```
                    Need strong consistency?
                           │
              ┌────────────┴────────────┐
              │ YES                     │ NO
              ▼                         ▼
       Need high throughput?      Use eventual consistency
              │                   (Cassandra, DynamoDB)
    ┌─────────┴─────────┐
    │ YES               │ NO
    ▼                   ▼
Shard data across      Use Raft
multiple Raft groups   (etcd, Consul)
(CockroachDB, TiDB)
```

### The Bottom Line

**Raft trades throughput for correctness.** It's designed for:
- Small clusters (3-7 nodes)
- Low-to-medium write volume (~10K writes/sec)
- Critical data that can NEVER be lost (config, metadata, coordination)

**Not designed for:**
- High-volume transactional data (use sharded Raft groups or different algorithm)
- High-throughput event streams (use Kafka)
- Cache/session data (use Redis)

## 2. How & Why Does It Work?

**Core Mechanism:**

- **Leader Election**: One server becomes the leader; followers replicate logs from the leader
- **Log Replication**: The leader appends log entries to followers; entries are replicated before being committed
- **State Machine**: Each server applies committed log entries to its state machine in order

**Why it works:**

- **Safety**: The algorithm guarantees that committed entries are never lost and are applied in the same order across all servers
- **Liveness**: The cluster makes progress as long as a majority of servers are alive
- **Simplicity**: Raft separates leader election, log replication, and safety concerns, making it easier to reason about than Paxos

## 3. Real-World Use Cases

- **Distributed Databases**: etcd, Consul, TiDB, ClickHouse
- **Key-Value Stores**: Couchbase, Riak
- **Service Mesh & Infrastructure**: Kubernetes (via etcd), Nomad, HashiCorp products
- **Message Queues**: NATS, LogDevice
- **Blockchain & Distributed Ledgers**: Some private blockchain implementations

### Case Study: How Raft Powers Kubernetes

#### The Problem

Kubernetes manages thousands of containers across hundreds of nodes. It needs to track:
- Which pods run on which nodes
- Service endpoints and configurations
- Secrets, ConfigMaps, deployments
- Node health and resource allocation

**What happens if this data is inconsistent or lost?**
- Pods get scheduled to dead nodes
- Services route traffic to non-existent containers
- Deployments fail or duplicate
- The entire cluster becomes unusable

#### The Solution: etcd + Raft

Kubernetes stores ALL cluster state in **etcd**, a key-value store that uses Raft.

```
┌─────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                    │
├─────────────────────────────────────────────────────────┤
│                                                          │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐ │
│   │  API Server │    │  API Server │    │  API Server │ │
│   └──────┬──────┘    └──────┬──────┘    └──────┬──────┘ │
│          │                  │                  │        │
│          └──────────────────┼──────────────────┘        │
│                             │                           │
│                             ▼                           │
│   ┌─────────────────────────────────────────────────┐   │
│   │              etcd Cluster (Raft)                │   │
│   │  ┌─────────┐   ┌─────────┐   ┌─────────┐       │   │
│   │  │ Node 1  │   │ Node 2  │   │ Node 3  │       │   │
│   │  │(Leader) │◄─►│(Follower│◄─►│(Follower│       │   │
│   │  └─────────┘   └─────────┘   └─────────┘       │   │
│   └─────────────────────────────────────────────────┘   │
│                             │                           │
│          ┌──────────────────┼──────────────────┐        │
│          ▼                  ▼                  ▼        │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐ │
│   │ Worker Node │    │ Worker Node │    │ Worker Node │ │
│   │ (100s-1000s)│    │   ...       │    │   ...       │ │
│   └─────────────┘    └─────────────┘    └─────────────┘ │
└─────────────────────────────────────────────────────────┘
```

#### How Raft Solves Each Problem

**1. Pod Scheduling (Strong Consistency)**
```
kubectl create deployment nginx --replicas=3

1. API Server receives request
2. Writes to etcd leader: "Create 3 nginx pods"
3. Raft replicates to majority (2/3 nodes)
4. Commit confirmed → Scheduler reads state
5. Scheduler assigns pods to nodes
```
**Why Raft?** Without consensus, two schedulers might read stale state and schedule the same pod twice.

**2. Node Failure (Automatic Failover)**
```
etcd Node 1 (Leader) crashes

1. Nodes 2 & 3 detect missing heartbeat (300-600ms)
2. Node 2 starts election, gets vote from Node 3
3. Node 2 becomes new leader
4. Cluster continues operating (no data lost)
```
**Why Raft?** Automatic leader election without human intervention. Kubernetes never loses cluster state.

**3. Network Partition (Split-Brain Prevention)**
```
Network splits: [Node 1] | [Node 2, Node 3]

Node 1 (old leader):
  - Cannot get majority (1/3)
  - Steps down, stops accepting writes

Node 2 & 3:
  - Elect new leader (2/3 majority)
  - Continue operating

Network heals:
  - Node 1 rejoins as follower
  - Syncs state from new leader
```
**Why Raft?** Majority requirement prevents two leaders. No split-brain, no data corruption.

**4. Configuration Changes (Zero Data Loss)**
```
kubectl apply -f new-config.yaml

1. Write to etcd leader
2. Leader replicates to followers
3. Wait for majority acknowledgment
4. Only then: "config applied successfully"
```
**Why Raft?** If leader crashes after user sees "success", data is safe on majority of nodes.

#### Why Not Just Use a Regular Database?

| Requirement | Regular DB | etcd + Raft |
|-------------|-----------|-------------|
| Automatic failover | Manual intervention | Automatic (leader election) |
| Split-brain prevention | Possible with bad config | Impossible (majority vote) |
| Strong consistency | Often eventual | Guaranteed (linearizable) |
| Watch/notify | Polling | Native watch API |

#### Key Numbers

- **etcd cluster size**: 3 or 5 nodes (never more)
- **Data stored**: ~1GB typical (just metadata, not app data)
- **Latency**: 1-10ms for writes
- **Throughput**: ~10,000 writes/sec (sufficient for control plane)

**The insight**: Kubernetes uses Raft for the **control plane** (small, critical data) while worker nodes handle the **data plane** (actual workloads). This separation lets Raft's strong consistency protect critical state without becoming a bottleneck.

### Raft vs Traditional SQL Replication

#### How SQL Handles Leader Outage (Poorly)

Traditional SQL doesn't have built-in consensus. Failover is messy:

| Mode | What Happens on Primary Failure |
|------|--------------------------------|
| **Async replication** | Replica promoted manually. Data loss (unreplicated writes gone). |
| **Semi-sync** | Waits for 1 replica ACK. Still needs external tool to promote. |
| **Sync** | Waits for ALL replicas. Still needs external failover tool. |

**The irony:** High-availability SQL tools (Patroni, Orchestrator) use **etcd or Consul** for leader election — which run **Raft**. SQL HA depends on consensus anyway.

#### SQL Replication vs Raft Quorum

Traditional SQL does **not** use quorum:

```
SQL Semi-Sync:
Primary → Replica 1 (ACK) → Commit ✓
       → Replica 2 (no wait)

Primary + Replica 1 die → Replica 2 promoted → 💥 Data loss

Raft:
Leader → Follower 1 (ACK) ─┐
      → Follower 2 (ACK) ─┼→ Majority → Commit ✓

Leader dies → Follower 1 or 2 elected → ✅ No data loss
```

| Aspect | SQL (Semi-Sync) | Raft (Quorum) |
|--------|-----------------|---------------|
| Waits for | 1 replica | Majority |
| Commit = durable? | No | **Yes** |
| Split-brain safe? | No | **Yes** |
| Auto failover? | No (needs tool) | **Yes** |

#### Which SQL Solutions Use Quorum?

| Solution | Uses Quorum? | Algorithm |
|----------|--------------|-----------|
| MySQL async/semi-sync | No | — |
| PostgreSQL streaming | No | — |
| **MySQL Group Replication** | **Yes** | Paxos |
| **Galera Cluster** | **Yes** | Certification-based |
| **CockroachDB** | **Yes** | Raft |
| **TiDB** | **Yes** | Raft |
| **Spanner** | **Yes** | Paxos |

**Key insight:** Distributed SQL with strong consistency uses Paxos/Raft underneath. You're not avoiding consensus — you're hiding it.

#### How Large-Scale Distributed SQL Works

Raft doesn't scale to 1000 nodes. So how do CockroachDB/TiDB/Spanner achieve large-scale strong consistency?

**Answer: Sharding with Raft per shard.**

```
1 billion rows across 1000 shards

┌────────────┐ ┌────────────┐ ┌────────────┐
│  Shard 1   │ │  Shard 2   │ │  Shard N   │
│ ┌────────┐ │ │ ┌────────┐ │ │ ┌────────┐ │
│ │  Raft  │ │ │ │  Raft  │ │ │ │  Raft  │ │
│ │3 nodes │ │ │ │3 nodes │ │ │ │3 nodes │ │
│ └────────┘ │ │ └────────┘ │ │ └────────┘ │
│  ~1M rows  │ │  ~1M rows  │ │  ~1M rows  │
└────────────┘ └────────────┘ └────────────┘
      ▲              ▲              ▲
      └──────────────┼──────────────┘
                     │
           Parallel writes to
           different shards
```

| Scaling Need | Solution |
|--------------|----------|
| More data | Add more shards |
| More throughput | Shards handle writes in parallel |
| Strong consistency | Raft within each shard |
| Cross-shard transactions | 2PC coordinates across Raft groups |

**You don't scale Raft to 1000 nodes. You run 1000 independent 3-node Raft groups.**

## 4. Distributed Environment: Concurrency, Consistency, Failures, Durability

| Aspect | How Raft Handles It |
|--------|-------------------|
| **Concurrency** | Commands are serialized through the log; leader ensures ordering |
| **Consistency** | Strong consistency: writes go through leader, committed entries applied in order |
| **Server Failures** | Tolerates up to (N-1)/2 failures in a cluster of N servers; lost data recovered from replicas |
| **Durability** | Log entries persisted to disk before replication; committed entries never lost |
| **Network Partitions** | Minority partition cannot make progress; majority partition continues operating |
| **Split Brain Prevention** | Leader election requires a majority vote; prevents conflicting leaders |

## 5. When Raft Cannot Be Used

- **High-Frequency Trading**: Latency-sensitive systems where consensus overhead is unacceptable
- **Extreme Scalability**: Not efficient for very large clusters (100+ nodes); performance degrades
- **Weak Consistency is Acceptable**: Systems tolerating eventual consistency should use cheaper alternatives (replication without consensus)
- **Byzantine Failures**: Raft assumes honest servers; doesn't protect against malicious/corrupted servers (use BFT algorithms instead)
- **Asynchronous Networks**: Raft requires timing assumptions; doesn't work in fully asynchronous environments
- **Geographically Distributed**: High latency between datacenters makes consensus slow; better to use eventual consistency with causal ordering
- **Real-Time Systems**: Hard deadlines requiring guaranteed response times

## 6. Where Raft Fits: The Consistency Spectrum

```
SPEED                                                      CORRECTNESS
  ◄─────────────────────────────────────────────────────────────►

  No Consensus          Weak Consensus        Strong Consensus
       │                      │                      │
       ▼                      ▼                      ▼
┌─────────────┐      ┌───────────────┐      ┌───────────────┐
│ Primary-    │      │ Quorum-based  │      │ Raft/Paxos    │
│ Backup      │      │ (W+R>N)       │      │               │
└─────────────┘      └───────────────┘      └───────────────┘
   Stock Mkt            DynamoDB              etcd, Consul
   Redis                Cassandra            CockroachDB
   MySQL async          MongoDB              TiDB
```

| Approach | Latency | Use Case | Example |
|----------|---------|----------|---------|
| **Primary-Backup** | Microseconds | Ultra-low latency, deterministic workloads | Stock exchanges, Redis |
| **Async Replication** | Milliseconds | High throughput, data loss acceptable | MySQL replication, Kafka |
| **Quorum (W+R>N)** | Low ms | Eventual consistency OK, high availability | DynamoDB, Cassandra |
| **Raft/Paxos** | 1-10ms | Strong consistency required, small clusters | etcd, Consul, Zookeeper |
| **2PC + Raft** | 10-100ms | Distributed ACID transactions | CockroachDB, Spanner |

**Raft's sweet spot:** Control plane coordination (leader election, config, metadata) for 3-7 nodes where strong consistency matters more than raw speed.

**Not Raft:** Data plane at scale (millions of items), ultra-low latency systems, or when eventual consistency suffices.

---

## Key Trade-offs

| Advantage | Disadvantage |
|-----------|-------------|
| Strong consistency | Higher latency (consensus overhead) |
| Simplicity & understandability | Limited scalability |
| Fault tolerance | Requires majority quorum (N/2 + 1) |
| Proven correctness | Slower in high-latency networks |

## Typical Cluster Size

Raft works best with 3, 5, or 7 servers. A 3-server cluster tolerates 1 failure; a 5-server cluster tolerates 2 failures. Larger clusters don't proportionally increase fault tolerance and incur higher coordination costs.

---

# Implementation Deep Dive

This section explains how our Go implementation works, with references to specific code.

## Code Structure

```
raft/
├── rpc.go      - Data structures, RPC messages, constants
├── raft.go     - Core Raft algorithm implementation
└── main.go     - Demo with key-value store application
```

## How to Run

```bash
cd /Users/rishav/Projects/system-design/algorithms/raft
go run *.go
```

## Architecture Overview

### Core Types (`rpc.go`)

**ServerState** (rpc.go:6-9)

- `Follower`: Passive node, receives updates from leader
- `Candidate`: Node seeking election as leader
- `Leader`: Handles all client requests, replicates to followers

**LogEntry** (rpc.go:21-25)

- `Term`: Election term when entry was created
- `Index`: Position in the log
- `Command`: Arbitrary command to apply (e.g., PUT key=value)

**RPC Messages** (rpc.go:27-49)

- `RequestVoteArgs/Reply`: Used during leader election
- `AppendEntriesArgs/Reply`: Used for heartbeats and log replication

**Timing Constants** (rpc.go:59-63)

- `HeartbeatInterval`: 100ms - Leader sends heartbeats to prevent elections
- `ElectionTimeoutMin/Max`: 300-600ms - Randomized to prevent split votes

---

## Step-by-Step Execution Flow

### Phase 1: Cluster Initialization (main.go:57-81)

**What happens:**

1. Creates 5 Raft nodes with apply channels (main.go:57-70)
2. Each node starts three background goroutines (raft.go:53-57):
   - `electionDaemon()` - Monitors election timeout
   - `heartbeatDaemon()` - Sends periodic heartbeats if leader
   - `applyDaemon()` - Applies committed entries to state machine

**Code walkthrough:**

```go
// main.go:71-73 - Each node wraps a Raft instance
for i := 0; i < numNodes; i++ {
    rafts[i] = NewRaft(i, rafts, applyChs[i])
}

// raft.go:53-57 - Background goroutines start immediately
go rf.electionDaemon()   // Checks if election timeout expired
go rf.heartbeatDaemon()  // Sends heartbeats if leader
go rf.applyDaemon()      // Applies committed entries
```

**Initial state (raft.go:39-47):**

- All nodes start as `Follower`
- `currentTerm = 0`
- `votedFor = -1` (haven't voted)
- `log = [dummy entry]` (index 0)
- Random election timeout set (raft.go:51)

---

### Phase 2: Leader Election (raft.go:87-150)

**Trigger:** Election timeout expires without hearing from leader

**Step 1: Timeout Detection** (raft.go:72-86)

```go
// electionDaemon runs every 50ms
if time.Since(rf.lastHeartbeat) > rf.electionTimeout {
    rf.startElection()  // Election timeout expired!
}
```

**Step 2: Become Candidate** (raft.go:89-94)

```go
rf.state = Candidate
rf.currentTerm++           // Increment term
rf.votedFor = rf.id        // Vote for self
rf.resetElectionTimeout()  // Randomize timeout
```

**Step 3: Request Votes** (raft.go:99-145)

Node sends `RequestVote` RPC to all peers in parallel:

```go
args := RequestVoteArgs{
    Term:         currentTerm,
    CandidateID:  candidateID,
    LastLogIndex: lastLogIndex,
    LastLogTerm:  lastLogTerm,
}
```

**Step 4: Peers Evaluate Vote** (raft.go:265-305)

Each peer checks (raft.go:280-303):

1. Is candidate's term ≥ my term? (raft.go:284-286)
2. Have I already voted in this term? (raft.go:289-291)
3. Is candidate's log at least as up-to-date? (raft.go:293-299)

If all checks pass → Grant vote:

```go
rf.votedFor = args.CandidateID
rf.resetElectionTimeout()  // Reset timeout (heard from valid candidate)
reply.VoteGranted = true
```

**Step 5: Win Election** (raft.go:119-139)

When candidate receives majority votes (3/5):

```go
majority := len(rf.peers)/2 + 1
if currentVotes >= majority && rf.state == Candidate {
    rf.becomeLeader()  // Won election!
}
```

**Step 6: Transition to Leader** (raft.go:142-158)

```go
rf.state = Leader
// Initialize leader state
rf.nextIndex[i] = len(rf.log)   // Next entry to send to each peer
rf.matchIndex[i] = 0            // Highest replicated entry per peer
```

**Why it works:**

- **Split vote prevention**: Randomized timeouts (300-600ms) make simultaneous elections unlikely
- **Safety**: Candidate with stale log cannot win (raft.go:296-299)
- **Liveness**: Followers reset timeout when granting votes, preventing disruption

---

### Phase 3: Heartbeats & Log Replication (raft.go:160-253)

**Heartbeat Mechanism** (raft.go:160-177)

Leader sends empty `AppendEntries` every 100ms:

```go
// heartbeatDaemon runs every 100ms
if rf.state == Leader {
    rf.replicateToAll()  // Send to all followers
}
```

**What followers do** (raft.go:307-350):

```go
// AppendEntries RPC handler
rf.resetElectionTimeout()  // Reset election timer (heard from leader)
rf.state = Follower        // Step down if candidate
```

This prevents followers from starting elections while leader is healthy.

---

### Phase 4: Client Request Processing (raft.go:26-41, main.go:30-33)

**Step 1: Client submits command** (main.go:91-92)

```go
kvStores[leaderID].Put("name", "Alice")
```

**Step 2: Leader appends to log** (raft.go:26-41)

```go
func (rf *Raft) Start(command interface{}) {
    if rf.state != Leader {
        return -1, rf.currentTerm, false  // Only leader accepts writes
    }

    entry := LogEntry{
        Term:    rf.currentTerm,
        Index:   len(rf.log),
        Command: command,
    }
    rf.log = append(rf.log, entry)

    go rf.replicateToAll()  // Trigger immediate replication
}
```

**Step 3: Replicate to followers** (raft.go:195-253)

For each follower, leader sends:

```go
args := AppendEntriesArgs{
    Term:         rf.currentTerm,
    LeaderID:     rf.id,
    PrevLogIndex: nextIdx - 1,           // Previous entry index
    PrevLogTerm:  rf.log[prevLogIndex].Term,  // Previous entry term
    Entries:      rf.log[nextIdx:],      // New entries to append
    LeaderCommit: rf.commitIndex,        // Leader's commit index
}
```

**Step 4: Follower validates and appends** (raft.go:329-348)

Follower checks:

1. Does my log have entry at `prevLogIndex` with matching `prevLogTerm`? (raft.go:334-336)
   - **NO** → Reply `Success=false` (log inconsistency)
   - **YES** → Append new entries (raft.go:339-348)

```go
// Log consistency check
if args.PrevLogIndex >= len(rf.log) ||
   rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
    return true  // Reject - logs don't match
}

// Append new entries
for i, entry := range args.Entries {
    index := args.PrevLogIndex + 1 + i
    rf.log = append(rf.log, entry)
}
```

**Step 5: Leader commits entry** (raft.go:231-243)

When majority of followers acknowledge:

```go
func (rf *Raft) updateCommitIndex() {
    for n := rf.commitIndex + 1; n < len(rf.log); n++ {
        count := 1  // Count self
        for i := range rf.peers {
            if rf.matchIndex[i] >= n {
                count++
            }
        }

        if count > len(rf.peers)/2 {
            rf.commitIndex = n  // Safe to commit!
        }
    }
}
```

**Step 6: Apply to state machine** (raft.go:245-263)

Background daemon applies committed entries:

```go
// applyDaemon runs every 10ms
for rf.lastApplied < rf.commitIndex {
    rf.lastApplied++
    entry := rf.log[rf.lastApplied]

    msg := ApplyMsg{
        CommandValid: true,
        Command:      entry.Command,
        CommandIndex: entry.Index,
    }

    rf.applyCh <- msg  // Send to application
}
```

**Step 7: Application processes command** (main.go:39-50)

```go
func (kv *KVStore) Apply(msg ApplyMsg) {
    cmd := msg.Command.(KVCommand)
    if cmd.Op == "put" {
        kv.data[cmd.Key] = cmd.Value  // Update key-value store
    }
}
```

**Why it works:**

- **Log consistency**: PrevLogIndex/PrevLogTerm check ensures logs match before appending
- **Commit safety**: Entry only committed when majority acknowledge (can't be lost)
- **Ordering**: All nodes apply entries in same order (same indices)

---

### Phase 5: Follower Failure (main.go:108-128)

**What happens when we kill Node 0:**

```go
rafts[followerID].Kill()  // Mark node as dead
```

**Impact:**

1. Node 0 stops responding to RPCs (raft.go:268, 312)
2. Leader continues replicating to remaining 4 nodes (raft.go:195-253)
3. Majority still available (3/5 ≥ 3 needed for quorum)
4. New entries still get committed:

```go
// Leader counts successful replications
count := 1  // Self
for i := range rf.peers {
    if rf.matchIndex[i] >= n {
        count++  // Node 0 won't increment this
    }
}
if count > len(rf.peers)/2 {  // 3 > 2.5 ✓ Still have majority!
    rf.commitIndex = n
}
```

**Result:** System continues operating normally with 4/5 nodes.

---

### Phase 6: Leader Failure & Re-election (main.go:133-142)

**What happens when we kill the leader:**

**Step 1: Leader dies**

```go
rafts[leaderID].Kill()  // Node 1 stops sending heartbeats
```

**Step 2: Followers detect failure** (raft.go:72-86)

After 300-600ms (random timeout per node):

```go
// Follower's electionDaemon detects timeout
if time.Since(rf.lastHeartbeat) > rf.electionTimeout {
    rf.startElection()  // No heartbeat received!
}
```

**Step 3: New election begins**

Suppose Node 3's timeout expires first:

```go
rf.state = Candidate
rf.currentTerm++  // Term 1 → Term 2
rf.votedFor = 3   // Vote for self
```

Node 3 requests votes from Nodes 2 and 4 (Nodes 0 and 1 are dead).

**Step 4: Win election**

Node 3 receives votes from:

- Itself (1 vote)
- Node 2 (1 vote)
- Node 4 (1 vote)

Total: 3/5 votes = majority ✓

```go
if currentVotes >= majority {
    rf.becomeLeader()  // Node 3 becomes new leader
}
```

**Step 5: Resume operations** (main.go:150-157)

Client now sends commands to Node 3:

```go
kvStores[newLeaderID].Put("recovered", "true")
```

Node 3 replicates to Nodes 2 and 4 (3/5 = majority).

**Why it works:**

- **Automatic failover**: No human intervention needed
- **Quorum guarantee**: New leader must have all committed entries (raft.go:296-299)
- **Term monotonicity**: Term increases ensure old leader can't come back and interfere

---

## Key Design Decisions

### 1. Randomized Election Timeouts (raft.go:60-67)

```go
timeout := time.Duration(min + rand.Intn(max-min)) * time.Millisecond
```

**Why:** Prevents split votes. If two nodes timeout simultaneously, they split votes and nobody wins. Random timeouts (300-600ms) make simultaneous elections rare.

### 2. Log Consistency Check (raft.go:334-336)

```go
if args.PrevLogIndex >= len(rf.log) ||
   rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
    return true  // Reject
}
```

**Why:** Ensures logs are identical before appending. If follower's log diverged (e.g., missed entries), leader backs up and replays from common point.

### 3. Commit Rule (raft.go:233-243)

```go
if count > len(rf.peers)/2 {
    rf.commitIndex = n
}
```

**Why:** Majority replication guarantees committed entries survive any minority failure. Even if leader crashes, new leader will have committed entries.

### 4. Separation of Concerns (raft.go:53-57)

- `electionDaemon`: Only handles election timeouts
- `heartbeatDaemon`: Only sends periodic heartbeats
- `applyDaemon`: Only applies committed entries

**Why:** Each goroutine has single responsibility. Easier to reason about, test, and debug.

---

## Concurrency Model

### Goroutines per Node

Each Raft node runs **3 background goroutines**:

1. **Election Monitor** (raft.go:72-86)
   - Wakes every 50ms
   - Checks if `time.Since(lastHeartbeat) > electionTimeout`
   - Starts election if timeout expired

2. **Heartbeat Sender** (raft.go:160-177)
   - Wakes every 100ms
   - If leader → sends AppendEntries to all followers
   - If follower → does nothing

3. **Commit Applier** (raft.go:245-263)
   - Wakes every 10ms
   - Checks if `lastApplied < commitIndex`
   - Sends committed entries to application via channel

### Synchronization

**Mutex Protection** (raft.go:15)

```go
rf.mu.Lock()
defer rf.mu.Unlock()
```

All Raft state is protected by a single mutex. Every function that reads/writes state holds this lock.

**Why single mutex:**

- Simple: No deadlocks, no lock ordering issues
- Correct: Atomic state transitions
- Fast enough: Lock held briefly (~microseconds)

**Channel Communication** (raft.go:17, main.go:77-82)

```go
rf.applyCh <- msg  // Raft → Application
```

Committed entries sent to application via Go channel. Decouples Raft from application logic.

---

## Testing Scenarios in Demo

### Scenario 1: Normal Operation (main.go:87-103)

- Leader accepts 3 commands
- All 5 nodes replicate and apply
- Demonstrates: Log replication, commit, state machine application

### Scenario 2: Follower Failure (main.go:108-128)

- Kill 1 follower
- Submit command
- 4/5 nodes still replicate (majority)
- Demonstrates: Fault tolerance, quorum-based commit

### Scenario 3: Leader Failure (main.go:133-142)

- Kill leader
- Followers detect timeout
- New leader elected
- Demonstrates: Automatic failover, leader election

### Scenario 4: Minority Cluster (main.go:150-157)

- Only 3/5 nodes alive
- New leader accepts commands
- Commands replicate to 2/3 alive nodes (majority of cluster)
- Demonstrates: Continued operation with minimum quorum

---

## Edge Cases Handled

1. **Stale Leader** (raft.go:219-225)
   - Old leader receives higher term → steps down to follower

   ```go
   if reply.Term > rf.currentTerm {
       rf.currentTerm = reply.Term
       rf.state = Follower
   }
   ```

2. **Conflicting Logs** (raft.go:339-348)
   - Follower has entry at index with different term → overwrite

   ```go
   if rf.log[index].Term != entry.Term {
       rf.log = rf.log[:index]  // Delete conflicting entry
       rf.log = append(rf.log, entry)
   }
   ```

3. **Split Vote** (raft.go:60-67)
   - Two candidates get equal votes → timeout → retry with new term
   - Randomized timeouts make repeated split votes unlikely

4. **Network Partition** (not explicitly tested, but handled)
   - Minority partition cannot elect leader (no majority)
   - Majority partition continues operating
   - When partition heals, minority nodes update to majority's log

---

## Performance Characteristics

### Latency

- **Normal case**: 1 RTT (leader → follower → leader)
- **Leader failure**: 300-600ms election timeout + 1 RTT for first command

### Throughput

- **Bottleneck**: Leader serializes all writes
- **Improvement**: Batch multiple commands in single AppendEntries (not implemented here)

### Scalability

- **Limited**: Every follower replication is sequential (raft.go:191-193)
- **Better approach**: Pipeline AppendEntries (not implemented here)

---

## Further Exploration

### What's Missing (not implemented for simplicity)

1. **Persistence** - Log should be written to disk (raft paper §5.2)
2. **Log Compaction** - Snapshots to prevent unbounded log growth (raft paper §7)
3. **Configuration Changes** - Adding/removing servers safely (raft paper §6)
4. **Optimizations** - Batching, pipelining, read-only queries

### Recommended Next Steps

1. **Add persistence**: Save log/term/votedFor to disk before replying to RPCs
2. **Test network partitions**: Use channels to simulate network failures
3. **Benchmark**: Measure commits/sec, latency percentiles
4. **Compare to production**: Read `hashicorp/raft` or `etcd/raft` source

### Study Questions for Interviews

1. Why can't Raft commit entries from previous terms by counting replicas? (raft paper Figure 8)
2. How does Raft handle read-only queries? (Hint: lease-based reads, read index)
3. What happens if two leaders exist in different terms? (Hint: higher term always wins)
4. How do you add a new server to the cluster? (Hint: joint consensus)

---

## References

- [Raft Paper](https://raft.github.io/raft.pdf) - Original research paper
- [Raft Visualization](https://raft.github.io/) - Interactive demo
- [etcd/raft](https://github.com/etcd-io/etcd/tree/main/raft) - Production implementation
- [hashicorp/raft](https://github.com/hashicorp/raft) - Another production implementation
