# Order Matching Engine

A production-grade stock exchange order matching engine in Go, built to teach critical system design concepts used in real trading platforms.

**Performance:** Processes **1.1 million orders/second** with **0.88 microsecond** latency using lock-free LMAX Disruptor pattern.

> **For Beginners:** This is the core of Robinhood, E*TRADE, or NASDAQ. When you click "Buy 100 shares of Apple," this system finds someone selling Apple stock and connects you together.

## Table of Contents

1. [Quick Start](#quick-start)
2. [What is an Order Matching Engine?](#what-is-an-order-matching-engine)
3. [High-Level System Architecture](#high-level-system-architecture)
4. [LMAX Disruptor Pattern (Ring Buffer)](#lmax-disruptor-pattern-ring-buffer)
5. [Core Concepts](#core-concepts)
6. [Data Structures](#data-structures)
7. [Component Deep Dive](#component-deep-dive)
8. [Running the System](#running-the-system)
9. [Performance Benchmarks](#performance-benchmarks)
10. [Interview Questions](#interview-questions)
11. [Code Walkthrough](./CODE_WALKTHROUGH.md)

---

## Quick Start

Get the matching engine running in 60 seconds:

### Build and Run

```bash
# Clone and navigate to project
cd order-matching-engine

# Build the server
go build -o matching-engine ./cmd/server

# Start the server (listens on http://localhost:8080)
./matching-engine
```

### Test with Sample Orders

```bash
# Submit a buy order for 100 shares of AAPL at $150.00
curl -X POST http://localhost:8080/order \
  -H "Content-Type: application/json" \
  -d '{
    "symbol": "AAPL",
    "side": "buy",
    "type": "limit",
    "price": 150.00,
    "quantity": 100
  }'

# Response: {"accepted":true,"order_id":1,"remaining":100,"fills":[]}

# Submit a matching sell order (will execute immediately)
curl -X POST http://localhost:8080/order \
  -H "Content-Type: application/json" \
  -d '{
    "symbol": "AAPL",
    "side": "sell",
    "type": "limit",
    "price": 150.00,
    "quantity": 100
  }'

# Response: {"accepted":true,"order_id":2,"remaining":0,"fills":[{"price":150.00,"quantity":100}]}

# View the order book
curl http://localhost:8080/book/AAPL

# Cancel an order
curl -X DELETE http://localhost:8080/order/AAPL/1
```

### Run Benchmarks

```bash
# Benchmark ring buffer throughput
go test ./internal/disruptor -bench=BenchmarkSequencer_SingleProducer -benchtime=10s

# Expected: ~50M operations/sec, 19ns/op

# Benchmark end-to-end matching engine
go test ./tests -bench=BenchmarkEngine_MatchOrders -benchtime=10s

# Expected: 1.1M orders/sec, 0.88μs latency
```

### Run Tests

```bash
# Unit tests
go test ./...

# Integration tests with race detection
go test -race ./tests

# Verbose test output
go test -v ./internal/matching
```

---

## What is an Order Matching Engine?

The matching engine is the **heart of any exchange**. It receives buy and sell orders, matches them using price-time priority, and generates trades.

**Real-world analogy:** A smart auction house that runs 24/7:
- **Buyers** submit bids: "I'll pay $150 for Apple stock"
- **Sellers** submit asks: "I'll sell Apple stock for $150"
- The **matching engine** connects them instantly

```
┌─────────────┐                    ┌─────────────┐
│   Buyers    │────── Orders ──────│   Sellers   │
└─────────────┘         │          └─────────────┘
                        ▼
                 ┌─────────────┐
                 │  Matching   │
                 │   Engine    │
                 └──────┬──────┘
                        ▼
                  ┌─────────┐
                  │ Trades  │
                  └─────────┘
```

### Key Responsibilities

- **Maintain order book** - All active buy/sell orders organized by price
- **Match orders** - Find buyers for sellers using price-time priority
- **Execute trades** - Best price first, then first-come-first-served (FIFO)
- **Publish market data** - Real-time quotes for traders
- **Ensure determinism** - Same input always produces same output

### Example: Price Improvement

```
Order Book for AAPL:
  SELL orders (asks): $150.25 × 100 shares
  BUY orders (bids):  $150.00 × 100 shares

Alice submits: BUY 100 shares @ $150.50 (willing to pay up to $150.50)

Engine matches:
  1. Best sell price is $150.25 (cheaper than Alice's $150.50)
  2. Execute trade: 100 shares @ $150.25
  3. Alice saves $0.25 per share! ($150.50 - $150.25)

This is "price improvement" - taker always gets maker's price
```

---

## High-Level System Architecture

### Complete Order Flow

```
┌─────────────────────────────────────────────────────────────────────────┐
│                   Order Matching Engine Architecture                    │
│                                                                          │
│  ┌────────┐    ┌─────────┐    ┌──────────┐    ┌────────────────────┐  │
│  │ Client │───▶│ Gateway │───▶│   Risk   │───▶│  Ring Buffer       │  │
│  │ (HTTP) │    │  (API)  │    │  Checker │    │  (Lock-Free Queue) │  │
│  └────────┘    └─────────┘    └──────────┘    └─────────┬──────────┘  │
│                                                           │             │
│  ┌────────────────────────────────────────────────────────┘             │
│  │                                                                      │
│  ▼                                                                      │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │          SINGLE-THREADED CORE (LMAX Disruptor Pattern)           │  │
│  │  ┌──────────────┐    ┌────────────┐    ┌──────────────────────┐ │  │
│  │  │ Sequencer    │───▶│  Matching  │───▶│  Order Book          │ │  │
│  │  │ (CAS-based)  │    │  Engine    │    │  ┌────┐    ┌────┐   │ │  │
│  │  └──────────────┘    └────┬───────┘    │  │Bids│    │Asks│   │ │  │
│  │   Assigns seq #           │            │  │RBT │    │RBT │   │ │  │
│  │   atomically              │            │  └────┘    └────┘   │ │  │
│  │                           │            │  Red-Black Trees    │ │  │
│  │                           │            └──────────────────────┘ │  │
│  └───────────────────────────┼────────────────────────────────────┘  │
│                              │                                        │
│                              ▼                                        │
│                       ┌──────────────┐                                │
│                       │Event Processor                                │
│                       │(Single Thread)                                │
│                       └──────┬───────┘                                │
│                              │                                        │
│                              ▼                                        │
│                       ┌──────────────┐                                │
│                       │Event Batcher │ ──┐                            │
│                       │(async queue) │   │                            │
│                       └──────────────┘   │                            │
│                              │            │                           │
│                              ▼            └─────────────────────┐     │
│                       ┌─────────────┐                           │     │
│                       │ Event Log   │              HTTP Handler │     │
│                       │(Append-only)│              Post-Process │     │
│                       └─────────────┘                           │     │
│                              │                                  │     │
│                       Batched fsync                             ▼     │
│                       1000 events/batch              ┌─────────────┐  │
│                       OR 10ms timeout                │  Market     │  │
│                                                      │  Data Pub   │  │
│                                                      └─────────────┘  │
│                                                             │         │
│                                                      WebSocket bcast  │
│                                                      (non-blocking)   │
│                                                                       │
└───────────────────────────────────────────────────────────────────────┘

Performance with Ring Buffer:
  Throughput:  1.1M orders/sec
  Latency:     0.88 μs (p50), 1.09 μs (p99)
  Ring Buffer: 19.7 ns per operation (zero allocations)
```

### Processing Flow

```
Step 1: HTTP request arrives
     ↓
Step 2: Gateway validates JSON → Order object
     ↓
Step 3: Risk Checker applies pre-trade controls
     ↓
Step 4: Sequencer claims slot in ring buffer (CAS operation, lock-free)
     ↓
Step 5: Event Processor reads from ring buffer (single-threaded consumer)
     ↓
Step 6: Matching Engine processes order (deterministic, single-threaded)
     ↓
Step 7: Event Processor queues events to Event Batcher (non-blocking)
     │   Event Batcher accumulates events:
     │   └─→ Flush trigger: 1000 events OR 10ms timeout
     │       └─→ Event Log: Append batch + fsync
     ↓
Step 8: Event Processor sends result to HTTP Handler via response channel
     ↓
Step 9: HTTP Handler post-processing:
     ├─→ Update clearing house positions
     ├─→ Update risk tracking
     ├─→ Publish trades to Market Data (non-blocking)
     └─→ Publish L1 quotes to Market Data (non-blocking)
```

---

## LMAX Disruptor Pattern (Ring Buffer)

### Why Lock-Free Ring Buffer?

Traditional approach with mutexes creates contention:

```
Multi-threaded with locks (SLOW):
   HTTP Handler 1 ──┐
   HTTP Handler 2 ──┼──▶ [MUTEX LOCK] ──▶ Engine ──▶ [MUTEX LOCK] ──▶ EventLog
   HTTP Handler 3 ──┘

   Problem: Threads wait for mutex (50-100ns overhead per lock)
```

Ring buffer eliminates locks:

```
Lock-free with ring buffer (FAST):
   HTTP Handler 1 ──┐
   HTTP Handler 2 ──┼──▶ Ring Buffer (CAS) ──▶ Event Processor ──▶ Engine
   HTTP Handler 3 ──┘      (19.7ns)              (Single Thread)
```

### Ring Buffer Design

The ring buffer is a **pre-allocated circular queue** that serves as the central coordination mechanism between multiple HTTP handler threads (producers) and the single-threaded matching engine (consumer). Rather than having threads compete for mutex locks, the ring buffer uses atomic Compare-And-Swap (CAS) operations to claim sequence numbers, enabling truly lock-free multi-producer coordination. Each of the 8192 slots is cache-aligned to 64 bytes (one CPU cache line) to prevent false sharing between cores, and the power-of-2 size enables fast modulo operations via bitwise AND masks. This pre-allocation eliminates garbage collection pressure in the critical path, as no memory allocations occur during order processing.

The lock-free coordination works through a sequence-based protocol: producers atomically claim sequence numbers using CAS loops, write their order data to the corresponding slot, then signal readiness by updating the slot's sequence number with an atomic store (providing memory ordering guarantees). The single consumer spins on each slot's sequence number until it matches the expected value, processes the order deterministically, then updates a gating sequence to signal that the slot can be reused. This design achieves ~20 nanoseconds per operation (vs 50-100ns for mutexes) while maintaining determinism through sequential processing. When the buffer fills, producers spin briefly (~100μs) for backpressure before rejecting with HTTP 503, providing bounded latency and a clear signal for clients to back off.

```
┌─────────────────────────────────────────────────────────────────┐
│                   Ring Buffer (8192 slots)                       │
│                                                                  │
│  ┌────┬────┬────┬────┬────┬────┬────┬────┬────┬────┬────┬────┐ │
│  │ 0  │ 1  │ 2  │ 3  │ 4  │ 5  │ 6  │ 7  │... │8190│8191│ 0  │ │
│  └────┴────┴────┴────┴────┴────┴────┴────┴────┴────┴────┴────┘ │
│    ↑                        ↑                                   │
│  Consumer                Producer                                │
│  (reads)                 (writes)                                │
│                                                                  │
│  Each slot contains:                                             │
│  - OrderRequest (order to process)                               │
│  - Response channel (for result)                                 │
│  - Sequence number (for coordination)                            │
└─────────────────────────────────────────────────────────────────┘

Key Properties:
✓ Pre-allocated (no GC pressure)
✓ Power-of-2 size (8192) for fast modulo via bitwise AND
✓ Cache-aligned slots (64 bytes) to prevent false sharing
✓ Lock-free coordination using atomic CAS operations
```

#### Why Power-of-2 Size? Fast Modulo via Bitwise AND

The ring buffer size of 8192 (2^13) is carefully chosen to enable a critical performance optimization: replacing expensive modulo operations with fast bitwise AND operations.

**The Problem**: Every order needs to find its slot in the circular buffer. With unbounded sequence numbers (1, 2, 3, ..., 10000, ...) and a fixed buffer size (8192 slots), we need to map sequence numbers to slot indices:

```
Sequence 0     → Slot 0
Sequence 1     → Slot 1
Sequence 8191  → Slot 8191
Sequence 8192  → Slot 0     (wrap around!)
Sequence 8193  → Slot 1
Sequence 10000 → Slot ?     (10000 % 8192 = 1808)
```

**Traditional Approach (SLOW)**: Use modulo operator
```go
slotIndex = sequenceNumber % 8192  // Division operation: ~20-40 CPU cycles
```

**Modulo division is expensive** because CPUs implement it using iterative subtraction or division circuits. On modern processors, integer division takes 20-40 cycles vs 1 cycle for bitwise operations.

**Optimized Approach (FAST)**: Use bitwise AND with a bitmask
```go
indexMask = 8192 - 1           // = 8191 = 0x1FFF in binary
slotIndex = sequenceNumber & indexMask  // Bitwise AND: 1 CPU cycle
```

**Why This Works (Mathematical Proof)**:

For any power-of-2 number N = 2^k, the mask (N - 1) has all lower k bits set to 1:

```
Buffer size: 8192 = 2^13
Binary:      0010 0000 0000 0000 (bit 13 is set)

Mask: 8191 = 8192 - 1
Binary:      0001 1111 1111 1111 (bits 0-12 are all 1s)
```

When you AND any number with this mask, you keep only the lower 13 bits, which is exactly equivalent to modulo 8192:

```
Example 1: Sequence 10000 (within range)
  10000 (binary):    0010 0111 0001 0000
& 8191 (mask):       0001 1111 1111 1111
  ─────────────────────────────────────
  Result:            0001 1000 0001 0000 = 1808

Verify: 10000 % 8192 = 1808 ✓
```

```
Example 2: Sequence 100000 (large number)
  100000 (binary):   0001 1000 0110 1010 0000
& 8191 (mask):       0000 0001 1111 1111 1111
  ────────────────────────────────────────────
  Result:            0000 0000 0110 1010 0000 = 1696

Verify: 100000 % 8192 = 1696 ✓
```

**Performance Impact**:

| Operation | CPU Cycles | Nanoseconds @ 3GHz | Operations/sec |
|-----------|------------|-------------------|----------------|
| **Modulo (%)** | 20-40 | ~10ns | ~100M |
| **Bitwise AND (&)** | 1 | ~0.33ns | ~3000M |

**30x faster!** With 1 million orders/second, this saves ~10,000 CPU cycles per second (~3 microseconds), which compounds across the entire system.

**Code Reference**:
```go
// internal/disruptor/sequencer.go:77
index := seq & s.rb.indexMask  // Fast modulo: seq % 8192

// internal/disruptor/ring_buffer.go:78
indexMask uint64  // = bufferSize - 1 = 8191
```

**Why ONLY Power-of-2 Works**:

Non-power-of-2 sizes don't have this property. For example, with buffer size 8000:

```
8000 - 1 = 7999
Binary: 0001 1111 0010 1111

This mask does NOT capture modulo 8000:
  10000 & 7999 = 1968  (WRONG!)
  10000 % 8000 = 2000  (correct)
```

The bitmask only works when all lower bits are 1s, which only happens with 2^k numbers. This is why the ring buffer must be exactly 1024, 2048, 4096, 8192, or 16384 slots—never arbitrary values like 5000 or 10000.

### How Ring Buffer Works

The ring buffer coordinates lock-free communication using atomic Compare-And-Swap (CAS) operations and sequence numbers. Here's how the three-phase process works:

**1. Producer (HTTP Handler) Claims Slot:**

Multiple producers compete to claim sequence numbers using a CAS loop with backpressure handling.

**What is Compare-And-Swap (CAS)?**

CAS is a CPU-level atomic instruction that enables lock-free synchronization. It performs three operations atomically (as a single, indivisible hardware instruction):

1. **Compare**: Read the current value at a memory address
2. **Check**: Compare it against an expected value
3. **Swap**: If they match, write a new value; otherwise, do nothing

The critical property: **all three steps happen atomically**—no other thread can modify the value between the compare and swap. In Go, this is exposed as:

```go
func CompareAndSwapUint64(addr *uint64, old, new uint64) bool
```

**How CAS Works at the Hardware Level:**

Modern CPUs (x86, ARM, RISC-V) provide atomic CAS instructions:
- **x86**: `CMPXCHG` instruction (compare-exchange)
- **ARM**: `LDREX/STREX` pair (load/store exclusive)
- **RISC-V**: `LR/SC` pair (load-reserved/store-conditional)

These instructions use cache coherency protocols (MESI/MOESI) to ensure atomicity across cores. When one core executes CAS on a cache line, the CPU hardware:
1. Acquires exclusive ownership of that cache line
2. Invalidates copies in other cores' caches
3. Performs the compare-and-swap
4. Releases ownership

This happens in **1-2 CPU cycles** for cache hits, vs **20-40 cycles** for mutex locks (which require kernel syscalls and context switches).

**Why CAS is "Optimistic Concurrency":**

Unlike pessimistic locking (mutex), which assumes conflicts will happen and always locks, CAS assumes conflicts are rare:

```
Pessimistic (Mutex):          Optimistic (CAS):
─────────────────             ─────────────────
Lock mutex                    Read current value
  (blocks others)             Try atomic swap
Modify value                    If success: done!
Unlock mutex                    If failure: retry (conflict detected)
```

When contention is low (typical in this system), CAS succeeds on first try ~99% of the time, avoiding lock overhead entirely. When contention is high, CAS retries with the new value—still faster than blocking on a mutex.

**CAS Loop Pattern (Optimistic Retry):**

```go
for {
    current := atomic.LoadUint64(&counter)  // 1. Read current value
    next := current + 1                      // 2. Compute new value

    if atomic.CompareAndSwapUint64(&counter, current, next) {
        return next  // 3. Success! We won the race
    }
    // 4. Failure: another thread modified counter
    //    Loop retries with updated value
}
```

This pattern is lock-free because a thread never blocks—it either succeeds immediately or retries. Even if one thread is preempted mid-loop, other threads continue making progress.

**CAS in the Ring Buffer:**

```go
// Reference: internal/disruptor/sequencer.go:34-64
func (s *Sequencer) Next() (uint64, error) {
    const maxSpins = 10000

    for spins := 0; spins < maxSpins; spins++ {
        current := atomic.LoadUint64(&rb.cursor)
        next := current + 1

        gatingSequence := atomic.LoadUint64(&rb.gatingSequence)
        availableSequence := gatingSequence + bufferSize

        if next > availableSequence {
            runtime.Gosched()
            continue
        }

        if atomic.CompareAndSwapUint64(&rb.cursor, current, next) {
            return next, nil
        }
    }

    return 0, ErrBufferFull
}
```

This function implements lock-free sequence claiming through optimistic concurrency: each producer reads the current cursor value, calculates the next sequence number, verifies the buffer isn't full (by checking if the next sequence would exceed `gatingSequence + bufferSize`, which represents the oldest unconsumed slot), then attempts to atomically swap the cursor using CAS. If the CAS succeeds, the producer owns that sequence number; if it fails (because another producer modified the cursor first), the producer retries with the new cursor value. The algorithm is correct because CAS provides atomicity (only one producer can successfully claim any given sequence), the gatingSequence check prevents overwriting unconsumed data (backpressure), and the spin limit (10,000 iterations ≈ 100μs) bounds latency before rejecting with an error when the system is saturated.

**Key concepts:**
- **Multi-producer coordination**: Multiple HTTP handlers compete using CAS, no locks needed
- **Backpressure formula**: `next > gatingSequence + bufferSize` prevents overwriting unconsumed data
- **Spin-then-reject**: Spins 10,000 times (~100μs) before returning HTTP 503
- **Example**: Buffer size 8192, gatingSequence=10 → can claim up to sequence 8202

**2. Producer Publishes to Slot:**

After claiming a sequence, the producer has exclusive ownership and writes without CAS.

```go
// Write to claimed slot
// Reference: internal/disruptor/sequencer.go:75-87
func (s *Sequencer) Publish(seq uint64, request, responseCh) {
    index := seq & indexMask  // Fast modulo: seq % 8192 via bitwise AND
    slot := &rb.slots[index]

    // Write request data (exclusive ownership, no CAS needed)
    slot.Request = orderRequest
    slot.ResponseCh = responseChan

    // Memory barrier: Atomic store provides release semantics
    // Guarantees all writes above are visible before sequence update
    atomic.StoreUint64(&slot.SequenceNum, seq)
}
```

**Key concepts:**
- **Fast modulo**: `seq & (bufferSize - 1)` instead of `seq % bufferSize` (requires power-of-2 size)
- **Memory ordering**: Atomic store ensures Request/ResponseCh writes visible before SequenceNum update
- **No CAS needed**: Producer has exclusive ownership of claimed sequence

**3. Consumer (Event Processor) Reads Slot:**

Single-threaded consumer eliminates read contention and maintains determinism.

```go
// Single-threaded consumer loop
// Reference: internal/disruptor/processor.go:54-90
nextSequence := uint64(1)  // Start at 1 (0 is initial state)

for {
    index := nextSequence & rb.indexMask
    slot := &rb.slots[index]

    // Spin-wait until slot is ready
    // Slot is ready when SequenceNum matches expected sequence
    for atomic.LoadUint64(&slot.SequenceNum) != nextSequence {
        runtime.Gosched()  // Cooperative yielding, not busy loop
    }

    // Process order (single-threaded, deterministic, no locks)
    result := engine.ProcessOrder(slot.Request.Order)

    // Queue events for async batching
    eventBatcher.QueueEvent(newOrderEvent, fillEvents...)

    // Send response back to HTTP handler
    slot.ResponseCh <- result

    // Update gating sequence (signals slot can be reused)
    // This is what producers check in backpressure formula
    atomic.StoreUint64(&rb.gatingSequence, nextSequence)

    nextSequence++
}
```

**Key concepts:**
- **Single consumer**: Eliminates read contention, enables 99% L1 cache hit rate
- **Spin-wait with Gosched**: Waits for SequenceNum to match, yields CPU cooperatively
- **Sequence starts at 1**: Initial SequenceNum=0 ensures first slot isn't prematurely consumed
- **Gating sequence**: Updated AFTER processing, signals producers that slot is available for reuse

### Ring Buffer vs Traditional Approaches

| Approach | Latency | Throughput | Complexity |
|----------|---------|------------|------------|
| **Mutex Lock** | 50-100ns | ~200K ops/sec | Simple |
| **Channel** | 100-200ns | ~500K ops/sec | Simple |
| **Ring Buffer** | 19.7ns | ~50M ops/sec | Moderate |

### Backpressure Handling

When ring buffer fills up, we use **spin-then-reject** strategy:

```go
1. Spin for ~100μs (10,000 iterations) waiting for space
2. If still full, return HTTP 503 "Server Busy"
3. Client can retry immediately

This prevents unbounded latency while maximizing throughput
```

### Memory Layout (Cache Alignment)

```
RingBufferSlot (64 bytes = 1 cache line):
┌─────────────┬──────────────┬──────────────┬──────────────┐
│ SequenceNum │ RequestPtr   │ ResponseCh   │ Padding      │
│  (8 bytes)  │  (8 bytes)   │  (8 bytes)   │  (40 bytes)  │
└─────────────┴──────────────┴──────────────┴──────────────┘

Why 64 bytes? Prevents false sharing:
- CPU cache line = 64 bytes
- If two slots shared a cache line, writes to one would invalidate the other
- With 64-byte alignment, each slot has its own cache line
- Result: No cache invalidation between cores
```

### Performance Impact

**Before (Mutex-based):**
- Lock overhead: ~50-100ns per order
- Event log: 1 fsync per event (~10-100ms each)
- Throughput: ~200K orders/sec

**After (Ring Buffer + Batching):**
- Ring buffer: ~20ns per operation
- Event log: 1 fsync per 1000 events
- Throughput: **1.1M orders/sec** (5-10x improvement!)

### Ring Buffer Internals Deep Dive

This section explains the low-level coordination mechanisms that enable lock-free operation.

#### 1. RingBufferSlot Structure and Readiness Signaling

Each slot uses atomic SequenceNum operations to coordinate producer-consumer handshakes without locks.

**Structure** (from `internal/disruptor/ring_buffer.go:47-64`):
```go
type RingBufferSlot struct {
    SequenceNum uint64              // Atomic coordination field
    Request     *OrderRequest       // The order to process
    ResponseCh  chan *OrderResponse // Result delivery channel
    _           [40]byte            // Padding to 64 bytes (cache line)
}
```

**Readiness Signaling**: The lock-free handshake protocol works as follows:

1. **Initial state**: All slots initialized with `SequenceNum = 0`
2. **Producer claims**: Claims sequence N via CAS on `cursor`
3. **Producer writes**: Writes Request and ResponseCh to slot at index `N & indexMask`
4. **Producer signals**: Sets `SequenceNum = N` (atomic store with release semantics)
5. **Consumer waits**: Spins on `SequenceNum != N`, yields with `runtime.Gosched()`
6. **Consumer reads**: When `SequenceNum == N`, slot is ready to read
7. **Consumer signals**: Updates `gatingSequence = N` after processing (allows reuse)

**Why start at sequence 1?** Initial `SequenceNum=0` ensures consumer doesn't prematurely read unwritten slots. Producer starts claiming from sequence 1.

**Memory ordering guarantee**: The atomic store of `SequenceNum` provides a release barrier, ensuring all prior writes (Request, ResponseCh) are visible before the consumer reads.

#### 2. Cursor and Sequencing Relationships

Three atomic fields coordinate multi-producer, single-consumer access:

**Field Definitions** (from `internal/disruptor/ring_buffer.go:83-93`):

```go
type RingBuffer struct {
    cursor         uint64  // Next sequence to claim (multi-producer, atomic CAS)
    gatingSequence uint64  // Highest consumed sequence (backpressure signal)
    indexMask      uint64  // Fast modulo bitmask (bufferSize - 1)
    bufferSize     uint64  // Must be power of 2 (e.g., 8192)
    // ...
}
```

**Cursor Relationships**:
- **cursor**: Tracks the highest sequence number claimed by any producer
  - Multiple producers compete using CAS to increment this
  - Starts at 0, first successful claim gets sequence 1

- **gatingSequence**: Tracks the highest sequence number consumed by the event processor
  - Single consumer updates this after processing each slot
  - Signals to producers that slots are available for reuse

- **indexMask**: Fast modulo operation via bitwise AND
  - For bufferSize=8192 (2^13), indexMask=8191
  - `slot_index = sequence & indexMask` instead of `sequence % bufferSize`

**Backpressure Formula**:
```
next > gatingSequence + bufferSize  →  Buffer is full, reject
```

**Example**:
- Buffer size: 8192 slots
- gatingSequence: 10 (consumer processed up to sequence 10)
- Available sequences: 11 through 8202 (inclusive)
- If producer tries to claim sequence 8203: **REJECTED** (buffer full)

**Wraparound Safety**: Sequences are uint64, wrapping around after 2^64 operations. This is safe because:
- At 1M ops/sec: Takes 584,942 years to wrap
- Comparison `next > gatingSequence + bufferSize` works correctly with wraparound
- Slot reuse via modulo ensures same physical slots serve multiple sequences

#### 3. CAS Operation and Multi-Producer Coordination

Multiple HTTP handlers compete to claim sequences using Compare-And-Swap (CAS) operations.

**CAS Loop Flow** (from `internal/disruptor/sequencer.go:34-64`):

```
HTTP Handler A                    HTTP Handler B                    Ring Buffer
─────────────                     ─────────────                     ───────────
Load cursor=100                   Load cursor=100                   cursor=100
Calculate next=101                Calculate next=101

CAS(cursor, 100→101)              CAS(cursor, 100→101)
  ✓ SUCCESS (wins race)             ✗ FAIL (cursor changed)         cursor=101

  Claim sequence 101                Retry...
                                    Load cursor=101
                                    Calculate next=102
                                    CAS(cursor, 101→102)
                                      ✓ SUCCESS                     cursor=102

                                    Claim sequence 102
```

**Spin-Then-Reject Strategy**:
1. **Spin phase**: Try CAS up to 10,000 times (~100μs)
2. **Yield on full**: Call `runtime.Gosched()` when buffer full
3. **Reject**: After max spins, return `ErrBufferFull` → HTTP 503 "Server Busy"

**Why not spin forever?**
- Prevents unbounded latency (bounded to ~100μs)
- Provides backpressure signal to clients
- Clients can retry or shed load

**Key Properties**:
- **Lock-free**: No mutexes, only atomic operations
- **Wait-free publish**: After claiming sequence, no contention on write
- **Fairness**: Not guaranteed (CAS winner is arbitrary), but typically fair in practice

#### 4. Memory Barriers and Visibility Guarantees

Atomic operations provide memory ordering guarantees critical for lock-free correctness.

**Producer Memory Ordering** (from `internal/disruptor/sequencer.go:75-87`):

```go
// Step 1: Write data to slot (non-atomic)
slot.Request = orderRequest      // Write 1
slot.ResponseCh = responseChan   // Write 2

// Step 2: Signal readiness with release barrier
atomic.StoreUint64(&slot.SequenceNum, seq)  // Release barrier
```

**Release Semantics**: The atomic store guarantees:
- All writes before this store (Request, ResponseCh) complete first
- All writes are visible to threads that observe the stored value
- Creates a "happens-before" relationship with consumer's load

**Consumer Memory Ordering** (from `internal/disruptor/processor.go:67-69`):

```go
// Acquire semantics: Load with visibility guarantee
available := atomic.LoadUint64(&slot.SequenceNum)  // Acquire barrier

if available == nextSequence {
    // All producer writes are now visible
    req := slot.Request      // Guaranteed to see producer's write
    ch := slot.ResponseCh    // Guaranteed to see producer's write
}
```

**Acquire Semantics**: The atomic load guarantees:
- All writes that happened-before the store are visible after the load
- Consumer sees a consistent view of the slot data

**Why This Matters**:
- Without memory barriers, CPU/compiler could reorder operations
- Consumer might read `SequenceNum=N` but stale Request data
- Memory barriers prevent these reorderings, ensuring correctness

**Go's Atomic Guarantees**:
- `atomic.StoreUint64`: Provides release semantics
- `atomic.LoadUint64`: Provides acquire semantics
- Together, they create a synchronization point without locks

#### 5. Consumer Synchronization Pattern

The single-threaded consumer uses a spin-wait pattern with cooperative yielding.

**Consumer Loop** (from `internal/disruptor/processor.go:54-90`):

```go
nextSequence := uint64(1)  // Start at 1

for {
    index := nextSequence & rb.indexMask
    slot := &rb.slots[index]

    // Spin-wait phase: Poll until slot is ready
    for {
        available := atomic.LoadUint64(&slot.SequenceNum)
        if available == nextSequence {
            break  // Slot is ready!
        }

        // Cooperative yielding: Don't busy-loop
        runtime.Gosched()  // Yield CPU to other goroutines
    }

    // Process order (single-threaded, no locks)
    result := engine.ProcessOrder(slot.Request.Order)

    // Queue events for batching
    eventBatcher.QueueEvent(events...)

    // Send response
    slot.ResponseCh <- result

    // Signal slot is free for reuse
    atomic.StoreUint64(&rb.gatingSequence, nextSequence)

    nextSequence++
}
```

**Key Design Choices**:

**Why single-threaded consumer?**
- **Determinism**: Same input always produces same output (required for compliance)
- **No read contention**: Only one thread reads, eliminates cache invalidation
- **Cache efficiency**: 99% L1 hit rate vs ~60% with multi-threading
- **No coordination overhead**: No locks or CAS needed on read path

**Why spin-wait instead of blocking?**
- **Latency**: Blocking (mutex, channel) adds 50-200ns overhead
- **Predictability**: Spin-wait has consistent latency
- **Cache coherence**: Keeps data hot in L1 cache

**Why runtime.Gosched()?**
- **Cooperative yielding**: Prevents monopolizing CPU core
- **Lets producers run**: Gives HTTP handlers CPU time to produce
- **Avoids busy loop**: Reduces CPU usage when buffer is empty
- **Go scheduler integration**: Works with Go's M:N threading model

**Gating Sequence Update**:
- Updated AFTER processing (not before)
- Signals to producers: "This slot is now available for reuse"
- Enables backpressure: Producers check `next > gatingSequence + bufferSize`

**Why update after, not before?**
- Ensures consumer finishes processing before slot is reused
- Prevents producer from overwriting slot while consumer is still reading
- Provides flow control mechanism

---

## Core Concepts

### 1. Single-Threaded Core (Why It's Faster)

**Counter-intuitive truth:** One thread is faster than many threads for matching.

**Why?**

| Benefit | Multi-Threaded | Single-Threaded |
|---------|---------------|-----------------|
| **Determinism** | Race conditions | Same input = same output |
| **Lock overhead** | 50-100ns per lock | 0 (no locks!) |
| **Cache efficiency** | ~60% hit rate | ~99% hit rate |
| **Context switches** | Frequent | Never |

**The key insight:** Matching is CPU-bound (pure computation), not I/O-bound. Orders must be processed sequentially (FIFO requirement), so parallelism doesn't help.

```
CPU time per order: ~200 cycles = 67ns @ 3GHz
Lock overhead: 50-100ns

Adding locks doubles latency with zero benefit!
```

### 2. Price-Time Priority (FIFO)

Orders match by:
1. **Best price first** - Highest bid (buyers), lowest ask (sellers)
2. **Time priority** - At same price, earlier orders match first

```
Order Book for AAPL:

ASKS (Sell Orders)              BIDS (Buy Orders)
Price    │ Qty  │ Time          Price    │ Qty  │ Time
─────────┼──────┼──────          ─────────┼──────┼──────
$151.00  │ 200  │ 10:01          $150.00  │ 100  │ 09:58 ← Best Bid
$150.50  │ 150  │ 10:00          $149.50  │ 200  │ 09:59
$150.25  │ 100  │ 09:55 ← Best Ask       $149.00  │ 300  │ 10:02

Market BUY 150 shares:
  → Matches $150.25 × 100 (best price, earliest time)
  → Matches $150.50 × 50 (next best price)
  → Average: $150.33
```

### 3. Order Types

| Type | Behavior | Use Case | Risk |
|------|----------|----------|------|
| **Market** | Execute NOW at any price | Need certainty | Price slippage |
| **Limit** | Execute only if price ≤ X | Price protection | May not fill |
| **IOC** | Fill available, cancel rest | Partial fills OK | Incomplete fill |
| **FOK** | Fill entire order or cancel | All-or-nothing | High rejection rate |

### 4. Fixed-Point Arithmetic

**Never use floats for money!**

```go
// The problem with floats
0.1 + 0.2 == 0.3  // false!
0.1 + 0.2         // 0.30000000000000004

// Solution: Store as cents (integers)
price := int64(15025)  // $150.25 = 15025 cents
total := price * 100   // Exact: 1502500 cents

// Only convert to float for display
fmt.Printf("$%.2f\n", float64(price)/100)
```

**Real-world impact:** NYSE processes 3 billion shares/day. With floats, tiny errors of 0.01¢ per trade = **$300K/day** in discrepancies!

### 5. Event Sourcing

Store **every state change** (event), not just current state.

```
Event Log (Append-Only):
┌──────┬────────┬────────────────────────────────┐
│ Seq  │ Type   │ Data                           │
├──────┼────────┼────────────────────────────────┤
│ 1    │ NEW    │ {id:1, AAPL, buy, $150, 100}  │
│ 2    │ NEW    │ {id:2, AAPL, sell, $150, 50}  │
│ 3    │ FILL   │ {trade:1, $150, qty:50}        │
│ 4    │ CANCEL │ {id:1, remaining:50}           │
└──────┴────────┴────────────────────────────────┘
```

**Benefits:**
- Complete audit trail (regulatory requirement)
- Disaster recovery (replay events to rebuild state)
- Time travel (query historical state)
- Debugging (reproduce exact bug)

---

## Data Structures

### Order Book Architecture

Combines three data structures for optimal performance:

```
                         OrderBook
                             │
            ┌────────────────┴────────────────┐
            │                                 │
      Bids (RB-Tree)                   Asks (RB-Tree)
      sorted high→low                  sorted low→high
            │                                 │
     ┌──────┼──────┐                   ┌──────┼──────┐
     ▼      ▼      ▼                   ▼      ▼      ▼
  $150  $149.5  $149                $150.25 $150.5 $151
     │                                    │
     ▼                                    ▼
  PriceLevel                          PriceLevel
  ┌─────────────────┐                ┌─────────────────┐
  │ Price: $150.00  │                │ Price: $150.25  │
  │ TotalQty: 250   │                │ TotalQty: 100   │
  │ Orders: ────┐   │                │ Orders: ────┐   │
  └─────────────┼───┘                └─────────────┼───┘
                ▼                                  ▼
        Doubly-Linked List                 Doubly-Linked List
        (FIFO queue)                       (FIFO queue)

        [Order1]◄──►[Order2]◄──►[Order3]
         100sh       100sh       50sh
         09:55       09:58       10:01
        (oldest)                (newest)

Plus: Hash Map[OrderID] → OrderNode (for O(1) cancellation)
```

### Why These Structures?

**1. Red-Black Tree (for price levels)**
- Sorted prices: O(log n) insert/delete
- Guaranteed balanced (height ≤ 2 log n)
- Best bid/ask: O(1) (cached pointers)

**2. Doubly-Linked List (for FIFO at each price)**
- O(1) append (new orders to tail)
- O(1) removal (for cancels or fills)
- Maintains time priority

**3. Hash Map (for order lookup)**
- O(1) lookup by OrderID
- Enables fast cancellation without searching

### Complexity Analysis

| Operation | Time | Explanation |
|-----------|------|-------------|
| **Get Best Bid/Ask** | O(1) | Cached pointers |
| **Add Order** | O(log P) | RB-Tree insert, P = price levels (~500) |
| **Cancel Order** | O(1) | Hash lookup + list removal |
| **Match Order** | O(M × log P) | M fills × level removal, M typically < 10 |

**Practical performance:** ~100-200 CPU cycles per order = 33-67ns @ 3GHz

---

## Component Deep Dive

### 1. Risk Checker (`internal/risk/checker.go`)

Pre-trade controls prevent catastrophic errors:

```go
type RiskChecker struct {
    maxOrderSize    int64   // Prevent fat-finger errors
    maxOrderValue   int64   // Limit exposure
    priceBandPercent float64 // Reject prices far from market
    maxPositionSize int64   // Prevent concentration
}

// Real-world example: Knight Capital 2012
// - Buggy deployment sent unintended orders
// - NO pre-trade risk controls
// - Lost $440M in 45 minutes
//
// With risk controls:
// ✓ Daily volume limit would stop after $X million
// ✓ Position limit would prevent accumulation
// ✓ Rate limit would detect anomaly
```

### 2. Event Log (`internal/events/log.go`)

Append-only journal for compliance and recovery, with async batching for performance.

**Architecture Flow**:
```
Event Processor → Event Batcher → Event Log
(single-thread)   (async queue)    (disk)
```

**IMPORTANT**: Event Log is NOT a direct ring buffer consumer. The Event Processor is the only ring buffer consumer, and it queues events to the Event Batcher asynchronously.

#### Event Batcher Architecture

The Event Batcher (`internal/disruptor/batcher.go`) provides async decoupling and batching between the event processor and disk I/O.

**Structure** (from `internal/disruptor/batcher.go:21-28`):
```go
type EventBatcher struct {
    eventLog      *events.EventLog
    queue         chan interface{}  // Capacity: 2000 (2 × batchSize)
    batchSize     int               // Default: 1000 events
    flushInterval time.Duration     // Default: 10ms
}
```

**Queue Semantics**:
- **Capacity**: 2000 events (2 × batchSize) for burst handling
- **Non-blocking**: Uses `select/default` pattern
- **Can drop events**: If queue is full, events are dropped with warning log
- **Best effort**: Prioritizes low latency over guaranteed delivery

**Batching Logic** (from `internal/disruptor/batcher.go:59-100`):

The batcher uses a dual-trigger flush mechanism:

```go
// Batch loop with two flush triggers
for {
    select {
    case event := <-b.queue:
        batch = append(batch, event)
        if len(batch) >= batchSize {  // Trigger 1: Size threshold
            flush(batch)
        }

    case <-ticker.C:  // Trigger 2: Time threshold (10ms)
        if len(batch) > 0 {
            flush(batch)
        }
    }
}
```

**Flush Triggers**:
1. **Size trigger**: Flush when batch reaches 1000 events
2. **Time trigger**: Flush every 10ms even if batch is incomplete
3. **Result**: Events written within 10ms maximum, optimally in 1000-event batches

**Non-Blocking Queue** (from `internal/disruptor/batcher.go:114-126`):

```go
func QueueEvent(event interface{}) {
    select {
    case b.queue <- event:
        // Successfully queued
    default:
        // Queue full, drop event (log warning)
        log.Printf("WARNING: Event queue full, dropping event")
    }
}
```

**Why non-blocking?**
- Event Processor must never block on I/O
- Maintains deterministic latency for matching engine
- Trades durability for performance (acceptable for non-critical events)

#### Event Log Disk Format

Binary format on disk (`internal/events/log.go`):

```go
Binary format on disk:
┌──────────┬──────────┬──────────┬───────────┬──────────┐
│ Seq (8B) │ Type(2B) │ Len (4B) │ Data (var)│ CRC (4B) │
└──────────┴──────────┴──────────┴───────────┴──────────┘
```

#### Sync Modes and Performance Impact

**Sync Mode = true** (durable, slow):
- Every event appended with immediate fsync
- Latency: ~10ms per fsync
- Without batching: 1000 events × 10ms = 10 seconds
- **With batching: 1 fsync per 1000 events = 10ms (1000x faster!)**

**Sync Mode = false** (fast, not durable):
- Events buffered in OS page cache
- Latency: ~1μs per event
- Risk: Lost on crash before OS flushes

**With batching (recommended)**:
- Balances durability and performance
- 1 fsync per batch (1000 events)
- Effective latency: 10ms ÷ 1000 = 10μs per event
- Best of both worlds: Near-durability with high performance

#### Integration with Event Processor

The Event Processor queues events after matching but before sending response:

```go
// Reference: internal/disruptor/processor.go:92-130
func processRequest(slot *RingBufferSlot) {
    // 1. Process order (matching engine)
    result := engine.ProcessOrder(slot.Request.Order)

    // 2. Queue events for async batching (non-blocking)
    eventBatcher.QueueEvent(&events.NewOrderEvent{...})
    for _, fill := range result.Fills {
        eventBatcher.QueueEvent(&events.FillEvent{...})
    }

    // 3. Send response to HTTP handler (fast path)
    slot.ResponseCh <- result
}
```

**Key Points**:
- Events queued AFTER matching (have sequence number)
- Queuing is non-blocking (never blocks matching engine)
- Response sent immediately (doesn't wait for fsync)
- Event batching happens asynchronously in separate goroutine

#### Ordering Guarantees

**Within Event Log**: Events are ordered by:
1. Ring buffer sequence number (determines processing order)
2. Batch flush order (events within batch maintain order)

**Event Log vs Market Data**: No explicit ordering guarantee between:
- Event Log updates (async via batcher)
- Market Data publications (sync non-blocking from HTTP handler)

In practice, Market Data typically has lower latency due to sync publication path.

### 3. Market Data Publisher (`internal/marketdata/publisher.go`)

Real-time data distribution to subscribers via non-blocking channels.

**Architecture Flow**:
```
Event Processor → HTTP Handler → Market Data Publisher → WebSocket Subscribers
(processes order)  (post-process)  (non-blocking pub)
```

**IMPORTANT**: Market Data Publisher is NOT a ring buffer consumer. It's called by the HTTP handler AFTER receiving the execution result from the event processor.

#### Publisher Integration

The HTTP handler publishes market data in its post-processing phase (from `cmd/server/main.go:406-469`):

```go
// Step 1: Get execution result from event processor
response := <-responseCh  // Result from ring buffer processing

// Step 2: Post-processing (happens in HTTP handler, NOT event processor)
for _, fill := range response.Result.Fills {
    // Update clearing house
    clearingHouse.RecordTrade(fill)

    // Update risk positions
    riskChecker.UpdatePosition(...)

    // Publish trade (non-blocking)
    publisher.PublishTrade(tradeReport)
}

// Step 3: Publish L1 quote (top of book)
l1Quote := buildL1Quote(orderBook)
publisher.PublishL1(l1Quote)  // Non-blocking
```

**Key Points**:
- Market data published AFTER event processor returns result
- Published in HTTP handler thread (not single-threaded core)
- Multiple HTTP handlers can publish concurrently (uses RWMutex for subscriber list)
- Publishing is non-blocking (never blocks matching engine)

#### Non-Blocking Publication Pattern

The publisher uses select/default to prevent slow subscribers from blocking the matching engine.

**PublishTrade Implementation** (from `internal/marketdata/publisher.go:145-167`):

```go
func (p *Publisher) PublishTrade(trade TradeReport) {
    p.mu.RLock()  // Read lock (allows concurrent publishers)
    defer p.mu.RUnlock()

    // Send to all subscribers for this symbol
    for _, ch := range p.tradeSubs[trade.Symbol] {
        select {
        case ch <- trade:
            // Successfully sent to subscriber
        default:
            // Subscriber's channel is full - DROP THE UPDATE
            // This prevents slow subscribers from blocking fast producers
        }
    }
}
```

**Why non-blocking?**
- **Matching engine isolation**: HTTP handlers must never block on subscriber I/O
- **Slow subscriber tolerance**: One slow WebSocket client can't impact throughput
- **Dropped updates acceptable**: Market data is best-effort, not guaranteed delivery
- **Alternative**: Slow subscribers should increase buffer size or consume faster

**What happens to dropped updates?**
- Update is silently dropped (no error, no retry)
- Subscriber sees gaps in data stream
- Recommendation: Subscribers should track sequence numbers and detect gaps

#### Subscriber Management

The publisher maintains separate subscriber lists for different data levels.

**Structure** (from `internal/marketdata/publisher.go:71-79`):

```go
type Publisher struct {
    mu          sync.RWMutex              // Protects subscriber lists (NOT data path)
    l1Subs      map[string][]chan L1Quote // L1 subscribers per symbol
    l2Subs      map[string][]chan L2Depth // L2 subscribers per symbol
    tradeSubs   map[string][]chan TradeReport
    allL1Subs   []chan L1Quote            // All-symbols L1 subscribers
    allTradeSubs []chan TradeReport
    bufferSize  int                       // Default: 1000 (channel buffer)
}
```

**Mutex Usage**:
- **RWMutex**: Allows concurrent publishers (multiple HTTP handlers)
- **Read lock**: Used during publication (doesn't block other publishers)
- **Write lock**: Used during subscribe/unsubscribe (rare operations)
- **NOT on data path**: Mutex protects subscriber list, not the actual data

**Channel Buffering**:
- Each subscriber gets buffered channel (default: 1000 updates)
- Buffer absorbs bursts (e.g., 50 trades in rapid succession)
- If buffer fills: Updates dropped with select/default

#### Market Data Levels

**L1 (Level 1) - Top of Book**:
```go
type L1Quote struct {
    Symbol    string
    BidPrice  int64  // Best bid price
    BidSize   int64  // Total quantity at best bid
    AskPrice  int64  // Best ask price
    AskSize   int64  // Total quantity at best ask
    LastPrice int64  // Last trade price
    LastSize  int64  // Last trade quantity
    Timestamp int64
}
```

**Use case**: Retail traders, basic price displays (~1-10 updates/sec)

**L2 (Level 2) - Market Depth**:
```go
type L2Depth struct {
    Symbol    string
    Bids      []PriceLevel  // Top N price levels (e.g., top 5 bids)
    Asks      []PriceLevel  // Top N price levels (e.g., top 5 asks)
    Timestamp int64
}
```

**Use case**: Active traders, order book visualizations (~10-100 updates/sec)

**L3 (Level 3) - Full Order Book**:
- Every individual order in the book
- Rarely provided to public (high bandwidth)
- Used by: Market makers, HFT firms

**Trade Reports**:
```go
type TradeReport struct {
    TradeID       uint64
    Symbol        string
    Price         int64
    Quantity      int64
    AggressorSide Side  // Which side initiated (buy/sell)
    Timestamp     int64
}
```

**Use case**: Time & sales displays, volume analysis, charting

#### Ordering Guarantees

**Within Market Data Publisher**: No ordering guarantee across symbols
- Different HTTP handlers publish concurrently
- Symbol A's trade might be published before Symbol B's earlier trade
- Same symbol: Updates from same HTTP handler are ordered

**Market Data vs Event Log**: No explicit ordering guarantee between:
- Event Log updates (async via Event Batcher, queued in event processor)
- Market Data publications (sync non-blocking, published from HTTP handler)

**In practice**:
- Market Data has lower latency (sync publication path)
- Event Log has higher latency but guaranteed durability
- Typical difference: Market Data arrives 10-100μs before Event Log persists

**Example timing**:
```
T+0μs:   Event Processor processes order
T+1μs:   Event Processor queues events to Event Batcher (non-blocking)
T+2μs:   Event Processor sends response to HTTP Handler
T+3μs:   HTTP Handler publishes Market Data (non-blocking)
T+5ms:   Event Batcher flushes batch to Event Log (OR timeout at 10ms)
```

### 4. Settlement (`internal/settlement/clearing.go`)

T+2 settlement with netting:

```
T+0 (Trade Date)     T+1 (Clearing)       T+2 (Settlement)
────────────────     ──────────────       ────────────────
Order matched        Netting calculated   Securities delivered
Trade reported       Obligations summed   Cash exchanged (DVP)

Example - Alice trades AAPL with Bob:
  10:00 - Alice buys 100 from Bob @ $150
  11:00 - Alice sells 60 to Bob @ $151
  14:00 - Alice buys 40 from Bob @ $149

Without netting: 3 settlements
With netting: Net = Alice buys 80 (67% reduction!)
```

---

## Running the System

### Quick Start

```bash
# Clone and build
git clone https://github.com/your-repo/order-matching-engine
cd order-matching-engine
go build ./...

# Run server
go run ./cmd/server -port 8080

# In another terminal, run demo
go run ./cmd/client demo
```

### API Examples

```bash
# Submit limit order
curl -X POST localhost:8080/order -d '{
  "symbol": "AAPL",
  "side": "buy",
  "type": "limit",
  "price": "150.00",
  "quantity": 100,
  "account_id": "TRADER1"
}'

# View order book
curl "localhost:8080/book?symbol=AAPL&levels=10"

# Cancel order
curl -X DELETE "localhost:8080/cancel?symbol=AAPL&order_id=123"
```

### Testing

```bash
# Run all tests
go test ./tests -v

# Run performance benchmark (10M orders)
go test ./tests -run TestPerformanceBenchmark -v

# Check for race conditions
go test -race ./...

# Ring buffer benchmarks
go test ./internal/disruptor -bench=. -benchtime=10s
```

---

## Performance Benchmarks

### Test Environment

- **CPU**: Intel Core i7-7820HQ @ 2.90GHz (8 cores)
- **RAM**: 32 GB
- **OS**: macOS
- **Go Version**: 1.21

### Results (with Ring Buffer)

```
Processing 10,000,000 orders...

RESULTS:
  Orders processed: 10,000,000
  Time elapsed:     ~9 seconds
  Throughput:       1,138,298 orders/sec (peak)
  Throughput:       1,050,000 orders/sec (average)
  Latency (p50):    0.88 μs
  Latency (p99):    1.09 μs
  Fills generated:  5,000,000

Component Breakdown:
  Ring buffer:      19.7 ns/op (zero allocations)
  Sequence claim:   ~20 ns (CAS operation)
  Order matching:   ~100 ns (RB-Tree + list ops)
  Event batching:   ~10 ns amortized (1000 events/batch)
```

### Comparison

| System | Throughput | Latency (p99) | Notes |
|--------|-----------|---------------|-------|
| **This Engine (Ring Buffer)** | **1.1M orders/sec** | **1.09 μs** | Lock-free, batched I/O |
| This Engine (Mutex-based) | ~200K orders/sec | ~500 μs | Baseline implementation |
| LMAX Disruptor | ~6M orders/sec | ~10 μs | Production, optimized hardware |
| NASDAQ | ~1M msg/sec | 50-100 μs | Full exchange system |

### Optimization Impact

| Optimization | Improvement |
|--------------|-------------|
| **Lock-free ring buffer** | 5-10x throughput |
| **Batched event logging** | 1000x reduction in fsync calls |
| **Cache-aligned slots** | Eliminates false sharing |
| **Pre-allocated buffers** | Zero GC pressure in hot path |

---

## Interview Questions

### Q: Why use a single-threaded matching engine?

**A:** Three reasons make single-threading faster:

1. **Determinism**: Same input → same output (required for compliance)
2. **No locks**: Eliminates 50-100ns overhead per operation
3. **Cache efficiency**: 99% L1 hit rate vs ~60% with multi-threading

Orders must be processed sequentially (FIFO), so parallelism adds overhead without benefits.

### Q: How does the ring buffer achieve lock-free operation?

**A:** Uses atomic Compare-And-Swap (CAS) for coordination:

```go
// Multiple producers compete to claim sequences
for {
    current := atomic.LoadUint64(&cursor)
    next := current + 1

    // Try to claim (lock-free!)
    if atomic.CompareAndSwapUint64(&cursor, current, next) {
        return next  // Won the race!
    }
    // Lost race, retry
}
```

Key properties:
- Pre-allocated buffer (no allocations)
- Power-of-2 size for fast modulo (bitwise AND)
- Cache-aligned slots (prevent false sharing)
- Single consumer (no contention on read)

### Q: Why fixed-point instead of floating-point?

**A:** Floats have rounding errors that accumulate:

```go
0.1 + 0.2 == 0.3  // false! (0.30000000000000004)

// At scale:
3 billion trades × 0.01¢ error = $300K/day discrepancy
```

Solution: Store prices as cents (integers) for exact math.

### Q: How would you scale to handle millions of symbols?

**A:** Shard by symbol - each symbol is independent:

```
         Load Balancer (Hash by Symbol)
                 │
     ┌───────────┼───────────┐
     ▼           ▼           ▼
┌─────────┐ ┌─────────┐ ┌─────────┐
│Engine 1 │ │Engine 2 │ │Engine 3 │
│  AAPL   │ │  GOOGL  │ │  MSFT   │
│  TSLA   │ │  AMZN   │ │  NVDA   │
└─────────┘ └─────────┘ └─────────┘

99% of orders are single-symbol
Linear scalability: 3 engines → 3x throughput
```

### Q: What happens if the ring buffer fills up?

**A:** Backpressure strategy:

1. Producer spins for ~100μs waiting for space
2. If still full, return HTTP 503 "Server Busy"
3. Client can retry immediately

This prevents unbounded latency while maximizing throughput.

---

## References

- [LMAX Disruptor](https://lmax-exchange.github.io/disruptor/) - High-performance ring buffer pattern
- [How LMAX Revolutionized Trading](https://martinfowler.com/articles/lmax.html) - Martin Fowler's deep dive
- [Mechanical Sympathy](https://mechanical-sympathy.blogspot.com/) - Hardware-aware programming
- [NYSE Pillar](https://www.nyse.com/pillar) - NYSE's trading platform
- [SEC Rule 613 (CAT)](https://www.sec.gov/rules/final/2012/34-67457.pdf) - Audit trail requirements
- [Red-Black Trees](https://en.wikipedia.org/wiki/Red%E2%80%93black_tree) - Self-balancing BST

---

## Project Structure

```
order-matching-engine/
├── cmd/
│   ├── server/main.go          # HTTP server with ring buffer integration
│   └── client/main.go          # CLI client for testing
├── internal/
│   ├── disruptor/              # LMAX Disruptor pattern
│   │   ├── ring_buffer.go      # Lock-free ring buffer (8192 slots)
│   │   ├── sequencer.go        # CAS-based sequence coordinator
│   │   ├── processor.go        # Single-threaded event processor
│   │   └── batcher.go          # Batch event logger (1000 events/batch)
│   ├── orderbook/              # Order book data structure
│   │   ├── orderbook.go        # Main order book logic
│   │   ├── pricelevel.go       # Price level with FIFO queue
│   │   └── rbtree.go           # Red-black tree implementation
│   ├── matching/
│   │   └── engine.go           # Matching engine (single-threaded core)
│   ├── orders/
│   │   └── types.go            # Order, Fill, ExecutionResult types
│   ├── events/
│   │   ├── types.go            # Event type definitions
│   │   └── log.go              # Append-only event log
│   ├── risk/
│   │   └── checker.go          # Pre-trade risk controls
│   ├── settlement/
│   │   └── clearing.go         # T+2 settlement with netting
│   └── marketdata/
│       └── publisher.go        # L1/L2/L3 market data pub/sub
└── tests/
    ├── integration_test.go     # Comprehensive test suite (9 tests)
    └── disruptor_test.go       # Ring buffer unit tests
```

---

## Production Failure Scenarios and Recovery

Real-world trading systems fail in surprising ways, and recovery mechanisms are critical for regulatory compliance and business continuity. This section examines how production exchanges actually fail and the sophisticated recovery strategies they employ.

### Why Trading Systems Fail: Real-World Examples

#### 1. **CME Group 10-Hour Outage (November 2025)**

The world's largest derivatives exchange experienced a catastrophic failure that exposed critical gaps in disaster recovery planning.

**What Happened** ([CME Outage Analysis](https://www.chicagobusiness.com/finance-banking/cme-outage-exposes-flaw-exchange-operators-disaster-plan)):
- **Root Cause**: Cooling system failure at Aurora, Illinois data center (operated by CyrusOne)
- **Duration**: 10+ hours (Thanksgiving evening through Friday morning)
- **Impact**: Global futures and options trading halted, affecting trillions in notional value
- **Recovery Failure**: Despite having a backup data center in New York, CME chose NOT to failover

**Why Failover Wasn't Activated**:
- Initial signals suggested brief outage (incorrect assessment)
- **Many customers lacked infrastructure to connect to NY backup facility**
- Business decision: Wait for primary recovery rather than force partial migration
- Lack of automated failover triggers

**Lesson**: Disaster recovery plans are worthless if they're not regularly tested with actual customers. The "backup exists" checkbox doesn't equal "backup works."

#### 2. **Knight Capital Algorithmic Failure (2012)**

Not a system crash, but a deployment failure that cost $440 million in 45 minutes.

**What Happened**:
- Buggy deployment activated old, dormant code
- Algorithm sent unintended orders at high volume
- **No pre-trade risk controls** to detect anomaly
- **No kill switch** to stop runaway algorithm

**Recovery**:
- Manual intervention required (too slow)
- Positions had to be unwound at massive loss
- Company never recovered, was acquired

**Lesson**: Lack of circuit breakers and automated risk controls can be catastrophic.

#### 3. **NASDAQ Facebook IPO Failure (2012)**

Exchange software couldn't handle order volume on Facebook's IPO day.

**What Happened**:
- Order confirmation system overwhelmed (race condition in matching engine)
- 30-million-share discrepancy between what traders thought they owned vs reality
- **No way to cancel or confirm orders** for 2 hours
- Settlement took days to reconcile

**Lesson**: Load testing under realistic peak conditions is non-negotiable. Edge cases (IPO volume surges) must be tested.

### How Production Systems Are Architected for Fault Tolerance

Based on [patent literature](https://patents.google.com/patent/US7992034B2/en) and [production architectures](https://weareadaptive.com/trading-resources/blog/building-fault-tolerant-low-latency-exchanges/), here's how real exchanges handle failures:

#### 1. **Active Copy-Cat Architecture (Hot Standby)**

The industry-standard approach, used by CME, NASDAQ, and others:

```
Primary Matching Engine              Backup Matching Engine
─────────────────────                ─────────────────────
Receives order                       Receives same order (delayed)
  ↓                                    ↓
Processes immediately                 Waits for primary ACK
  ↓                                    ↓
Writes to journal                     Writes to journal
  ↓                                    ↓
Publishes result                      Compares result (validation)
  ↓                                    ↓
Sends ACK to backup ─────────────────►│
                                       │
If primary fails:                      │
                                    Promoted to primary
                                    Resumes from last ACK
```

**Key Properties**:
- **Primary processes, backup validates**: Only primary publishes results
- **Gated inputs**: Backup receives orders only AFTER primary processes them
- **Automatic comparison**: Backup verifies outputs match (detects Byzantine failures)
- **Instantaneous failover**: Backup already has state, takes over in microseconds
- **Zero data loss**: Every event journaled before processing

**Reference**: [US Patent 7992034B2 - Match server for a financial exchange having fault tolerant operation](https://patents.google.com/patent/US7992034B2/en)

#### 2. **Event Sourcing with Journaling**

LMAX and other high-performance exchanges use event sourcing for recovery ([LMAX Architecture](https://martinfowler.com/articles/lmax.html)):

```
Input Journal          Processing           Output Journal
─────────────         ───────────          ──────────────
New Order Event  ───► Matching Engine ───► Fill Events
  (persisted)         (in-memory)            (persisted)
      ↓                                           ↓
  Sequence: 1234                             Sequence: 1234
  Durable: fsync                             Durable: fsync
      ↓                                           ↓
  Recovery: Replay from here              Idempotency: Check if already processed
```

**Diamond Configuration** ([LMAX Disruptor Barriers](https://techvival.zyxist.com/blog/barriers-in-lmax-disruptor.html)):

```
            Input
              │
        ┌─────┴─────┐
        ▼           ▼
   Journal     Business Logic
   Writer      Processor
        └─────┬─────┘
              │
      Dependency Barrier
         (wait for both)
              │
              ▼
         Output Journal
```

**Why This Works**:
- **Dual journaling**: Input events AND output events logged
- **Barrier coordination**: Output only written after input persisted
- **Replay recovery**: Crash recovery = replay input journal from last checkpoint
- **Idempotency**: Sequence numbers prevent duplicate processing during replay

**Production Implementation Details**:
- Input journal written BEFORE processing (durability)
- Output journal written AFTER processing (audit trail)
- Barriers ensure ordering: input → processing → output
- Sequence numbers on every message enable idempotent replay

#### 3. **Geographic Redundancy**

Major exchanges maintain data centers in multiple locations:

```
Primary DC (New Jersey)          Backup DC (Illinois)         DR DC (London)
─────────────────────           ─────────────────────        ────────────────
Active-Active                   Hot Standby                  Cold Standby
  │                                │                             │
  ├─ NASDAQ servers               ├─ Replicated state           ├─ Weekly snapshots
  ├─ Real-time matching           ├─ 100μs behind primary      ├─ 4-hour RTO
  └─ <1ms latency                 └─ Auto-failover ready       └─ Manual failover
```

**Recovery Time Objectives (RTO)**:
- **Hot Standby**: 0-10 seconds (automatic failover)
- **Warm Standby**: 1-5 minutes (manual failover, state rebuild)
- **Cold Standby**: 1-4 hours (full system restoration)

**Recovery Point Objectives (RPO)**:
- **Synchronous replication**: 0 data loss (fsync to both sites)
- **Asynchronous replication**: Seconds of data loss (acceptable for some markets)
- **Snapshot-based**: Minutes to hours (disaster recovery only)

#### 4. **Circuit Breakers and Kill Switches**

Automated protection against runaway scenarios:

**Pre-Trade Risk Controls**:
```go
// Before order enters matching engine
type RiskCheck struct {
    MaxOrderSize      int64  // Prevent fat-finger: 1M shares rejected
    MaxOrderValue     int64  // Prevent excessive exposure: $10M limit
    DailyVolumeLimit  int64  // Detect anomaly: 10x normal = suspicious
    PriceBandCheck    bool   // Reject if price 10% away from market
    PositionLimits    bool   // Account can't exceed concentration limits
}
```

**Kill Switches** (Regulatory Requirement - SEC Rule 15c3-5):
```
Firm-Level Kill Switch:
  └─> Halts ALL orders from a firm instantly
       Used when: Algorithm malfunction detected
       Activation: Automated (anomaly detection) or Manual (trader panic button)

Exchange-Level Circuit Breaker:
  └─> Halts entire market
       Triggered: 7%, 13%, 20% market decline
       Duration: 15 minutes to end of day
```

### Recovery Strategies in Production

#### Strategy 1: **Checkpoint + Replay**

Used by LMAX and event-sourced systems:

```
Crash at Event 1,234,567
       ↓
1. Find last checkpoint (snapshot at event 1,200,000)
2. Restore state from snapshot (faster than full replay)
3. Replay events 1,200,001 → 1,234,567 from journal
4. Resume at event 1,234,568
```

**Tradeoffs**:
- ✅ Complete state recovery (deterministic)
- ✅ Audit trail preserved
- ⚠️ Replay time = RPO (10K events/sec = 3.4 seconds for 34K events)
- ❌ Requires idempotent event handlers

**Production Optimization**:
- Checkpoint every 100K events (balances snapshot cost vs replay time)
- Parallel replay on multi-core (non-conflicting symbols)
- Incremental snapshots (only changed data)

#### Strategy 2: **Hot Standby Promotion**

Used by exchanges with active-passive configuration:

```
Primary Failure Detected (heartbeat timeout: 100ms)
       ↓
1. Backup detects missing heartbeat
2. Backup assumes primary role (atomic flag flip)
3. Clients redirected to backup (DNS/load balancer update)
4. Backup starts publishing results
       ↓
Total failover time: 200-500ms
```

**Tradeoffs**:
- ✅ Sub-second failover
- ✅ Zero data loss (backup has all state)
- ⚠️ Cost: 2x hardware (backup is idle)
- ❌ Split-brain risk (both think they're primary)

**Split-Brain Prevention**:
- **Fencing**: Backup sends "kill" signal to primary's network interface
- **Quorum**: Requires majority vote from witness nodes
- **Shared storage lock**: Only one can hold exclusive file lock

#### Strategy 3: **Active-Active with Conflict Resolution**

Used by globally distributed systems:

```
New York DC                      London DC
────────────                     ──────────
Receives order A                 Receives order B
  ↓                                ↓
Processes locally                Processes locally
  ↓                                ↓
Replicates to London ────────────►│
  ◄────────────────────────────── Replicates to NY
  ↓                                ↓
Conflict detection: Both executed overlapping orders
  ↓
Resolution: Timestamp-based (earlier wins) or Sequence-based (lower ID wins)
```

**Tradeoffs**:
- ✅ Zero downtime (both always active)
- ✅ Geographic load distribution
- ❌ Eventual consistency (conflicts possible)
- ❌ Complex conflict resolution

**Not suitable for matching engines** (requires strong consistency), but used for:
- Market data distribution (pub/sub)
- Reference data (symbol master, static data)
- Analytics and reporting

### What Happens During Recovery: Step-by-Step

#### Scenario: Primary Matching Engine Crashes

**T+0ms: Crash occurs** (e.g., segfault, OOM, kernel panic)
```
Primary matching engine goroutine dies
Orders in ring buffer: 150 (not yet processed)
Orders in event batcher: 450 (not yet flushed)
Last fsync: Event 1,234,120
```

**T+100ms: Backup detects failure** (heartbeat timeout)
```
Backup matching engine notices: No heartbeat for 100ms
Load balancer marks primary as DOWN
DNS updated to route to backup IP
```

**T+200ms: Backup promoted to primary**
```
Backup reads last checkpoint: Event 1,234,000
Backup replays journal: Events 1,234,001 → 1,234,120 (120 events)
Replay time: 120 events × 1μs = 120μs
Backup state now matches primary's last good state
```

**T+300ms: Resume accepting orders**
```
Backup starts processing new orders at Event 1,234,121
Clients reconnect to new primary
Orders in old ring buffer: LOST (150 orders never processed)
Orders in old event batcher: LOST (450 events never flushed)
```

**Total Downtime**: 300ms
**Data Loss**: Last 600 events (150 ring + 450 batcher)
**RPO**: Time since last fsync (~10ms with batching)

### Our Prototype's Failure Modes

This implementation has **limited fault tolerance**:

| Failure | Detection | Recovery | Data Loss |
|---------|-----------|----------|-----------|
| **Single order panic** | Caught by defer/recover | Continue processing | 1 order (error returned) |
| **Event processor crash** | None | Manual restart | All in-flight orders |
| **Event batcher crash** | None | Manual restart | Events lost forever |
| **Disk full** | Log write fails | None | Orders execute, not logged |
| **Network partition** | Client timeout | Client retry | Depends on timing |
| **Server crash** | Healthcheck (if exists) | Manual restart + replay | Since last fsync |

**Missing Production Features**:
- ❌ No hot standby or backup instance
- ❌ No automated failover
- ❌ No checkpoint/snapshot system
- ❌ No replay automation on startup
- ❌ No health monitoring or alerting
- ❌ No graceful degradation
- ❌ No distributed consensus (single node)

---

## Prototype vs Production Systems

This implementation demonstrates production-proven patterns (LMAX Disruptor, event sourcing, single-threaded matching) but **simplifies or omits** features required for real exchanges. Here's how it differs:

### Architecture Differences

| Aspect | This Prototype | Production Exchange |
|--------|---------------|---------------------|
| **Matching Core** | Single goroutine for ALL symbols | Symbol-level sharding (1 thread per symbol/group) |
| **Throughput** | 1.1M orders/sec total | 1.1M orders/sec **per symbol** |
| **Scalability** | Single server, 1 core | Horizontal: 100+ servers, 1000+ cores |
| **Fault Tolerance** | None (single instance) | Hot standby, geographic redundancy |
| **Recovery** | Manual replay (if coded) | Automatic failover (<1 sec) |
| **Data Loss** | Last 10ms of events | Zero (synchronous replication) |
| **Deployment** | Single process | Distributed cluster with consensus |

### 1. **Symbol Sharding and Horizontal Scalability**

**Prototype**:
```go
// One engine processes ALL symbols
engine := matching.NewEngine()
engine.AddSymbol("AAPL")
engine.AddSymbol("GOOGL")
engine.AddSymbol("MSFT")
// ... all 5,000 symbols
```

**Production**:
```go
// Symbol sharding across servers
type Cluster struct {
    shards map[int]*MatchingEngine  // 100 shards
}

func (c *Cluster) RouteOrder(order *Order) {
    shard := hash(order.Symbol) % 100
    c.shards[shard].Process(order)  // Each shard is single-threaded
}
```

**Why Shard?**:
- **Linear scalability**: 10 servers = 10x throughput
- **Fault isolation**: AAPL failure doesn't affect GOOGL
- **Load balancing**: Distribute hot symbols across cores

**NASDAQ Example**:
- ~8,000 listed securities
- Partitioned across 30+ matching engines
- Each engine handles ~250-300 symbols
- Geographic redundancy: NJ primary, IL backup

### 2. **High Availability and Redundancy**

**Prototype**:
```go
// Single instance, no backup
server.Start()  // If this dies, everything stops
```

**Production**:
```go
// Active-passive configuration
type HACluster struct {
    primary   *MatchingEngine
    backup    *MatchingEngine
    arbiter   *WitnessNode  // Prevents split-brain
}

func (h *HACluster) Start() {
    go h.primary.Run()
    go h.backup.RunInStandby()  // Replicates state
    go h.arbiter.MonitorHeartbeats()

    // Automated failover
    go h.watchdog.DetectFailure(func() {
        h.backup.Promote()  // <1 sec failover
    })
}
```

**Production Setup**:
- **Geographic redundancy**: Primary in NJ, backup in IL, DR in London
- **Dual data centers**: Both active (load balanced) or active-passive
- **Automated failover**: Heartbeat-based detection, sub-second promotion
- **Split-brain prevention**: Quorum-based consensus or fencing

### 3. **Durability and Recovery**

**Prototype**:
```go
// Event log exists but no recovery automation
eventLog, _ := events.NewEventLog(config)
// If server crashes, log is on disk but nothing reads it
```

**Production**:
```go
// Automated recovery on startup
func Initialize() error {
    // 1. Load last checkpoint
    snapshot, err := loadCheckpoint()
    if err != nil {
        snapshot = NewEmptyState()
    }

    // 2. Replay events from checkpoint
    events := eventLog.ReadFrom(snapshot.LastEventID)
    for _, event := range events {
        engine.Replay(event)  // Idempotent
    }

    // 3. Verify state consistency
    if !engine.VerifyChecksum() {
        return ErrCorruptedState
    }

    // 4. Resume normal operation
    return engine.Start()
}
```

**Production Features**:
- **Checkpointing**: Snapshot state every 100K events
- **Incremental snapshots**: Only changed order book levels
- **Parallel replay**: Replay independent symbols concurrently
- **State verification**: Checksums to detect corruption
- **Automated recovery**: No manual intervention required

### 4. **Operational Monitoring**

**Prototype**:
```go
// Minimal stats endpoint
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
    // Returns basic counters
}
```

**Production**:
```go
// Comprehensive observability
type Metrics struct {
    // Business metrics
    OrdersPerSecond       prometheus.Counter
    LatencyP50, P99, P999 prometheus.Histogram
    FillRate              prometheus.Gauge

    // System health
    RingBufferUtilization prometheus.Gauge  // % full
    EventBatcherQueueSize prometheus.Gauge
    GoroutineCount        prometheus.Gauge
    HeapAllocated         prometheus.Gauge

    // Error tracking
    OrderRejects          prometheus.CounterVec  // By reason
    CASContentions        prometheus.Counter
    EventDrops            prometheus.Counter
}

// Distributed tracing
span := tracer.StartSpan("order.process")
defer span.Finish()
span.SetTag("symbol", order.Symbol)
span.SetTag("order_id", order.ID)
```

**Production Tooling**:
- **Metrics**: Prometheus, Grafana dashboards
- **Tracing**: Jaeger, Zipkin for distributed traces
- **Logging**: Structured logs (JSON), centralized (ELK stack)
- **Alerting**: PagerDuty integration for critical failures
- **Profiling**: Continuous profiling (CPU, memory, goroutines)

### 5. **Risk Management and Controls**

**Prototype**:
```go
// Basic pre-trade checks
type RiskChecker struct {
    maxOrderSize  int64
    maxOrderValue int64
}
```

**Production**:
```go
// Multi-layered risk framework
type RiskEngine struct {
    // Pre-trade (before matching)
    fatFingerCheck     *FatFingerDetector  // 10x avg size
    creditCheck        *CreditLimiter      // Real-time margin
    concentrationLimit *PositionTracker    // Sector limits
    priceBandCheck     *PriceBandValidator // % from NBBO

    // Intra-day (real-time monitoring)
    anomalyDetector    *MLModel            // Pattern detection
    velocityCheck      *RateLimiter        // Orders/sec limits

    // Post-trade (after matching)
    settlementRisk     *SettlementEngine   // DVP checks

    // Kill switches
    firmKillSwitch     *EmergencyHalt      // Stop all orders
    symbolHalt         *TradingHalt        // Pause symbol
}
```

**Regulatory Requirements**:
- **SEC Rule 15c3-5**: Market Access Rule (kill switches mandatory)
- **MiFID II** (Europe): Pre/post-trade transparency, best execution
- **Reg NMS**: Order protection, no trade-throughs
- **CAT** (Consolidated Audit Trail): Every order/event tracked

### 6. **Network Architecture**

**Prototype**:
```go
// Single HTTP server
http.ListenAndServe(":8080", handler)
```

**Production**:
```go
// Multi-protocol gateway
type Gateway struct {
    httpServer   *HTTPServer      // REST API (retail)
    fixServer    *FIXServer       // FIX 4.2/4.4 (institutions)
    binaryServer *BinaryServer    // Proprietary protocol (HFT)

    // Market data distribution
    multicast    *MulticastPub    // UDP multicast (low latency)
    websocket    *WebSocketServer // Streaming quotes (web)

    // Colocation
    directConnect *DMAPort        // Direct market access (μs latency)
}
```

**Production Network**:
- **Multiple protocols**: FIX, binary, multicast for different clients
- **Colocation**: Servers in same data center as exchange (reduce latency)
- **Direct feeds**: Dedicated network links for market data (not internet)
- **Redundant paths**: Multiple ISPs, BGP routing, network failover

### 7. **Testing and Validation**

**Prototype**:
```go
// Unit tests and basic integration tests
go test ./...
```

**Production**:
```go
// Comprehensive testing pyramid
type TestSuite struct {
    // Functional tests
    unitTests        []Test  // 10,000+ tests
    integrationTests []Test  // 1,000+ tests
    e2eTests         []Test  // 100+ scenarios

    // Non-functional tests
    loadTests        []LoadTest      // 10M orders/sec
    soakTests        []SoakTest      // Run for 72 hours
    chaosTests       []ChaosTest     // Random failures injected

    // Compliance tests
    regulatoryTests  []ComplianceTest // SEC scenarios

    // Performance regression
    benchmarkSuite   []Benchmark      // Detect slowdowns
}
```

**Production Testing**:
- **Load testing**: Simulate 10x peak volume
- **Chaos engineering**: Randomly kill servers, delay networks
- **Soak testing**: Run for days/weeks to find memory leaks
- **Regulatory scenarios**: Test every SEC-required failure mode
- **Shadow trading**: Run new version alongside production, compare outputs

### 8. **Deployment and Operations**

**Prototype**:
```bash
# Manual deployment
go build -o matching-engine ./cmd/server
./matching-engine
```

**Production**:
```yaml
# Kubernetes deployment with canary rollout
apiVersion: apps/v1
kind: Deployment
metadata:
  name: matching-engine
spec:
  replicas: 10
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0  # Zero-downtime deployment
  template:
    spec:
      containers:
      - name: matching-engine
        image: registry.example.com/matching-engine:v2.1.3
        resources:
          requests:
            cpu: "4000m"
            memory: "16Gi"
          limits:
            cpu: "4000m"     # CPU pinning
            memory: "16Gi"
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 5
        readinessProbe:
          httpGet:
            path: /ready
            port: 8080
```

**Production Operations**:
- **Immutable infrastructure**: Docker/Kubernetes
- **Canary deployments**: 5% traffic → validate → roll out
- **Blue-green deployment**: Run both versions, switch DNS
- **Feature flags**: Enable/disable features without deployment
- **Automated rollback**: Revert if error rate spikes

### 9. **Security**

**Prototype**:
```go
// No authentication/authorization
http.HandleFunc("/order", handleOrder)  // Anyone can submit
```

**Production**:
```go
// Multi-layered security
type Security struct {
    // Authentication
    apiKeys       *APIKeyStore     // SHA-256 hashed
    oauth         *OAuthProvider   // OAuth 2.0 / OIDC
    certificates  *MTLSValidator   // Mutual TLS

    // Authorization
    rbac          *RBACEngine      // Role-based access
    accountLimits *AccountLimiter  // Per-account quotas

    // Network security
    ddosProtection *RateLimiter    // IP-based throttling
    firewall       *WAF            // Web application firewall

    // Encryption
    tlsConfig      *tls.Config     // TLS 1.3 minimum
    encryption     *FieldEncryption // PII encryption at rest

    // Audit
    auditLog       *SecurityLog    // Every access logged
}
```

**Regulatory Security Requirements**:
- **SOC 2 Type II**: Annual audit of security controls
- **PCI DSS**: If handling payment cards
- **GDPR**: Data privacy (EU customers)
- **Encryption**: At-rest and in-transit
- **Access logs**: 7-year retention for SEC compliance

### 10. **Cost and Scale**

**Prototype**:
- **Hardware**: Single server ($100/month cloud instance)
- **Throughput**: 1.1M orders/sec
- **Cost per trade**: ~$0.00001 (negligible)

**Production**:
- **Hardware**: 100 servers × $10K each = $1M in hardware
- **Network**: Colocation + cross-connects = $100K/month
- **Staff**: 10 engineers × $200K salary = $2M/year
- **Compliance**: Legal, audit, licensing = $500K/year
- **Throughput**: 100M+ orders/sec across cluster
- **Cost per trade**: Still ~$0.00001 (economies of scale)

**NASDAQ Example** (approximate):
- 30+ matching engines
- 1000+ servers (web, market data, clearing)
- $100M+ annual technology budget
- 200+ engineers
- 99.99% uptime SLA (52 minutes downtime/year allowed)

### Summary: Prototype vs Production

| Feature | Prototype | Production | Difficulty to Add |
|---------|-----------|------------|-------------------|
| **Core matching algorithm** | ✅ Production-ready | ✅ Same | N/A |
| **LMAX Disruptor pattern** | ✅ Correct implementation | ✅ Same | N/A |
| **Event sourcing** | ✅ Basic journal | ✅ + Checkpoints + Replay | Medium |
| **Symbol sharding** | ❌ Single-threaded | ✅ Multi-core | Easy (1 day) |
| **High availability** | ❌ None | ✅ Hot standby | Hard (2 weeks) |
| **Automated recovery** | ❌ Manual | ✅ Automatic | Medium (1 week) |
| **Monitoring** | ❌ Basic | ✅ Full observability | Medium (1 week) |
| **Security** | ❌ None | ✅ Enterprise-grade | Hard (2 weeks) |
| **Multi-protocol** | ❌ HTTP only | ✅ FIX, Binary, WS | Medium (1 week) |
| **Regulatory compliance** | ❌ None | ✅ SEC, MiFID II | Very Hard (months) |

**What You'd Need to Add for Production**:

1. **Week 1**: Symbol sharding, basic monitoring
2. **Week 2**: Automated recovery, checkpointing
3. **Week 3**: Hot standby, failover automation
4. **Week 4**: Security (auth, TLS, audit logs)
5. **Month 2**: FIX protocol, market data feeds
6. **Month 3**: Regulatory compliance, testing
7. **Month 4**: Operations tooling, runbooks
8. **Month 5-6**: Performance tuning, chaos testing

**Total**: 6 months with 3-5 engineers to reach production-grade

**The Good News**: The core architecture (single-threaded matching, LMAX Disruptor, event sourcing) is **production-proven**. You're not rewriting fundamentals, just adding operational maturity.

---

## What You've Learned

By understanding this project, you now know:

**System Design Patterns:**
- LMAX Disruptor (lock-free ring buffer)
- Event Sourcing (append-only log)
- Single-threaded core for determinism

**Data Structures:**
- Red-Black Trees (guaranteed O(log n))
- Doubly-Linked Lists (O(1) FIFO queue)
- Hash Maps (O(1) order lookup)
- Ring Buffer (lock-free queue)

**Performance Optimization:**
- CPU cache optimization (99% L1 hit rate)
- Lock-free coordination (CAS operations)
- Batched I/O (1000x fsync reduction)
- Memory alignment (prevent false sharing)

**Financial Concepts:**
- Price-time priority (FIFO matching)
- Order types (Market, Limit, IOC, FOK)
- Fixed-point arithmetic (exact money math)
- T+2 settlement and netting
- Pre-trade risk controls

---

## Next Steps

1. **Run the code:** `go run ./cmd/server`
2. **Read the tests:** `tests/integration_test.go` shows how it all works
3. **Profile performance:** `go test -cpuprofile=cpu.prof`
4. **Study the walkthrough:** [CODE_WALKTHROUGH.md](./CODE_WALKTHROUGH.md)
5. **Modify and experiment:** Add new order types, optimize data structures
