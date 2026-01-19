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
│              ┌───────────────┴───────────────┐                        │
│              ▼                               ▼                        │
│       ┌─────────────┐                 ┌─────────────┐                │
│       │ Event Log   │                 │  Market     │                │
│       │ (Batched)   │                 │  Data Pub   │                │
│       └─────────────┘                 └─────────────┘                │
│              │                               │                        │
│       Append-only log              WebSocket broadcast               │
│       1000 events/batch            L1/L2/L3 quotes                   │
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
Step 5: Event Processor reads from ring buffer (single thread)
     ↓
Step 6: Matching Engine processes order (deterministic, single-threaded)
     ↓
Step 7: Results split:
     ├─→ Event Batcher (batches 1000 events → 1 fsync)
     └─→ Market Data Publisher (WebSocket broadcast)
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

The ring buffer is a **pre-allocated circular queue** that enables lock-free communication between producers (HTTP handlers) and consumer (event processor).

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

### How Ring Buffer Works

**1. Producer (HTTP Handler) Claims Slot:**

```go
// Claim sequence using CAS (Compare-And-Swap)
func (s *Sequencer) Next() (uint64, error) {
    for {
        current := atomic.LoadUint64(&rb.cursor)
        next := current + 1

        // Check if buffer is full
        if next > atomic.LoadUint64(&rb.gatingSequence) + bufferSize {
            return 0, ErrBufferFull  // Backpressure!
        }

        // Try to claim this sequence (lock-free!)
        if atomic.CompareAndSwapUint64(&rb.cursor, current, next) {
            return next, nil  // Successfully claimed!
        }
        // CAS failed, another thread won, retry
    }
}
```

**2. Producer Publishes to Slot:**

```go
// Write to claimed slot
index := sequence & indexMask  // Fast modulo: seq % 8192
slot := &rb.slots[index]

slot.Request = orderRequest
slot.ResponseCh = responseChan

// Memory barrier: Make writes visible to consumer
atomic.StoreUint64(&slot.SequenceNum, sequence)
```

**3. Consumer (Event Processor) Reads Slot:**

```go
// Single-threaded consumer loop
nextSequence := uint64(1)
for {
    index := nextSequence & rb.indexMask
    slot := &rb.slots[index]

    // Spin-wait until slot is ready
    for atomic.LoadUint64(&slot.SequenceNum) != nextSequence {
        runtime.Gosched()  // Yield to other goroutines
    }

    // Process order (single-threaded, deterministic)
    result := engine.ProcessOrder(slot.Request.Order)

    // Send response back to HTTP handler
    slot.ResponseCh <- result

    // Update gating sequence (allows slot to be reused)
    atomic.StoreUint64(&rb.gatingSequence, nextSequence)

    nextSequence++
}
```

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

Append-only journal for compliance and recovery:

```go
Binary format on disk:
┌──────────┬──────────┬──────────┬───────────┬──────────┐
│ Seq (8B) │ Type(2B) │ Len (4B) │ Data (var)│ CRC (4B) │
└──────────┴──────────┴──────────┴───────────┴──────────┘

With batching:
  - 1000 events buffered
  - Single fsync per batch
  - Reduction: 1000 fsync calls → 1 fsync call
  - Improvement: 1000x reduction in I/O overhead!
```

### 3. Market Data Publisher (`internal/marketdata/publisher.go`)

Real-time data distribution:

```go
type L1Quote struct {  // Top of book
    Symbol    string
    BidPrice, BidSize int64
    AskPrice, AskSize int64
    LastPrice, LastSize int64
}

// Non-blocking publish (doesn't block matching engine)
select {
case ch <- quote:
    // Success
default:
    // Channel full - drop update
    // Slow subscriber doesn't block fast producers
}
```

**Distribution levels:**
- **L1** (Top of book): Retail traders, ~1 update/sec
- **L2** (Depth): Active traders, ~10 updates/sec
- **L3** (Full book): Market makers, ~100 updates/sec

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
