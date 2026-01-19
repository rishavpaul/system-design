# Code Walkthrough: Order Matching Engine

> **For developers who want to understand the implementation:** This guide walks through the actual code execution paths, component interactions, and critical design patterns used in the system.

## Table of Contents

1. [System Architecture Overview](#system-architecture-overview)
2. [Component Interaction Flow](#component-interaction-flow)
3. [Critical Code Patterns](#critical-code-patterns)
4. [Execution Example: Limit Order](#execution-example-limit-order)
5. [Execution Example: Order Cancellation](#execution-example-order-cancellation)
6. [Data Structure Deep Dive](#data-structure-deep-dive)

---

## System Architecture Overview

### Component Map

```
order-matching-engine/
├── cmd/server/main.go          → HTTP Gateway, orchestrates components
├── internal/matching/engine.go → Single-threaded matching core
├── internal/orderbook/         → Order book data structures
│   ├── orderbook.go           → RB-Tree + Hash Map wrapper
│   ├── pricelevel.go          → FIFO queue per price level
│   └── rbtree.go              → Red-black tree implementation
├── internal/risk/checker.go    → Pre-trade risk controls
├── internal/events/log.go      → Append-only event log
├── internal/settlement/        → T+2 settlement simulation
└── internal/marketdata/        → Real-time data publishing
```

**📁 Browse the code:**
- [cmd/server/main.go](./cmd/server/main.go) - HTTP server and gateway
- [internal/matching/engine.go](./internal/matching/engine.go) - Core matching engine
- [internal/orderbook/orderbook.go](./internal/orderbook/orderbook.go) - Order book implementation
- [internal/orderbook/pricelevel.go](./internal/orderbook/pricelevel.go) - Price level FIFO queue
- [internal/orderbook/rbtree.go](./internal/orderbook/rbtree.go) - Red-black tree
- [internal/risk/checker.go](./internal/risk/checker.go) - Risk management
- [internal/events/log.go](./internal/events/log.go) - Event sourcing
- [internal/settlement/clearing.go](./internal/settlement/clearing.go) - Settlement
- [internal/marketdata/publisher.go](./internal/marketdata/publisher.go) - Market data

### High-Level Data Flow

```
HTTP POST /order
    ↓
┌─────────────────────────────────────────────────┐
│ Gateway (server/main.go)                        │
│ • Parse JSON → Order struct                     │
│ • Convert price string → int64 cents            │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ Risk Checker (risk/checker.go)                  │
│ • Validate size, value, price bands             │
│ • Check position & volume limits                │
│ • REJECT if fails, else continue                │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ Matching Engine (matching/engine.go)            │
│ ⚡ SINGLE-THREADED CORE                         │
│                                                  │
│ ProcessOrder() {                                │
│   • Validate order                              │
│   • Assign sequence # and order ID              │
│   • matchOrder() → Generate fills               │
│   • AddOrder() if remaining qty                 │
│ }                                                │
└────────────────┬────────────────────────────────┘
                 ↓
         ┌───────┴────────┐
         ↓                ↓
┌──────────────┐   ┌──────────────┐
│  Event Log   │   │ Market Data  │
│  • Append    │   │ • PublishL1  │
│  • Sync      │   │ • PublishTrade│
└──────────────┘   └──────────────┘
```

---

## Component Interaction Flow

### 1. Order Submission Path

**Entry Point:** `handleOrder()` in [`cmd/server/main.go`](./cmd/server/main.go#L181)

```
User submits: {"symbol": "AAPL", "side": "buy", "price": "150.50", "quantity": 100}
    ↓
handleOrder() {
    1. json.Decode() → OrderRequest struct
    2. Parse side: "buy" → orders.SideBuy
    3. Parse price: "150.50" → 15050 (cents as int64)
    4. Create orders.Order{...}
}
    ↓
riskChecker.Check(order) {
    // Pre-trade validation
    ✓ order.Quantity < maxOrderSize
    ✓ order.Value < maxOrderValue
    ✓ price within band (reference ± 10%)
    ✓ position < maxPosition
    → Returns: RiskResult{Passed: true/false}
}
    ↓
s.mu.Lock()  // Critical: Single-threaded matching
engine.ProcessOrder(order)
s.mu.Unlock()
    ↓
[Post-processing - See section below]
```

### 2. Matching Engine Core

**Function:** `ProcessOrder()` in [`internal/matching/engine.go`](./internal/matching/engine.go#L83)

```
ProcessOrder(order) {
    // Step 1: Validate
    book = orderBooks[order.Symbol]
    if book == nil → REJECT
    if order.Quantity <= 0 → REJECT

    // Step 2: Assign IDs (atomic counters)
    order.ID = NextOrderID()          // atomic.AddUint64()
    order.SequenceNum = nextSequence() // Global ordering
    order.Timestamp = Now()

    // Step 3: Match
    fills = matchOrder(order, book)

    // Step 4: Handle remaining
    if order.RemainingQty() > 0 {
        switch order.Type {
            Limit → book.AddOrder(order)  // Rest in book
            Market → CANCEL (no liquidity)
            IOC → CANCEL (immediate-or-cancel)
            FOK → CANCEL (already checked)
        }
    }

    return ExecutionResult{fills, order, ...}
}
```

### 3. Post-Processing Pipeline

```
After matching completes:

┌─────────────────────────────────────────┐
│ For each fill:                          │
│   • eventLog.Append(FillEvent)          │
│   • clearingHouse.RecordTrade()         │
│   • riskChecker.UpdatePosition()        │
│   • publisher.PublishTrade()            │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│ Update market data:                     │
│   • Get best bid/ask from book          │
│   • publisher.PublishL1(quote)          │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│ Return HTTP response:                   │
│   • Order ID, status, fills             │
│   • JSON to client                      │
└─────────────────────────────────────────┘
```

---

## Critical Code Patterns

### Pattern 1: Matching Both Sides with Closures

**Function:** `matchOrder()` in [`internal/matching/engine.go`](./internal/matching/engine.go#L162)

One matching loop handles both buy and sell orders using closures:

```go
// Determine matching side dynamically
if order.Side == Buy {
    getMatchLevel = book.GetBestAsk           // Buyer matches against sellers
    priceAcceptable = func(p int64) bool {
        return p <= order.Price               // Ask must be ≤ our limit
    }
} else {
    getMatchLevel = book.GetBestBid           // Seller matches against buyers
    priceAcceptable = func(p int64) bool {
        return p >= order.Price               // Bid must be ≥ our limit
    }
}

// Single matching loop for both sides
for order.RemainingQty() > 0 {
    level = getMatchLevel()                   // Get best price
    if level == nil { break }                 // No liquidity
    if !priceAcceptable(level.Price) { break } // Price doesn't cross

    // Match against FIFO queue at this level
    matchAtLevel(order, level)
}
```

**Why this works:**
- Closures capture the correct comparison logic
- No code duplication for buy vs sell
- Clean separation of concerns

---

### Pattern 2: FIFO Queue Walk + Price Improvement

**Function:** `matchOrder()` iterates through orders at each price level

```go
// Walk FIFO queue at current price level
for node := level.Head(); node != nil && order.RemainingQty() > 0; {
    makerOrder = node.Order
    nextNode = node.Next()  // Save before potential removal

    // Calculate fill
    fillQty = min(order.RemainingQty(), makerOrder.RemainingQty())

    // CRITICAL: Execute at maker's price (resting order sets price)
    fill = Fill{
        Price: level.Price,        // NOT taker's limit price
        Quantity: fillQty,
        MakerOrderID: makerOrder.ID,
        TakerOrderID: order.ID,
    }

    // Update quantities
    order.FilledQty += fillQty
    makerOrder.FilledQty += fillQty

    // Remove fully filled orders
    if makerOrder.IsFilled() {
        book.CancelOrder(makerOrder.ID)  // O(1) hash lookup + removal
    } else {
        level.UpdateQuantity(-fillQty)   // Update total for depth
    }

    node = nextNode
}
```

**Key concepts:**
- **FIFO enforcement:** `level.Head()` always returns oldest order
- **Price improvement:** Taker gets maker's (potentially better) price
- **Safe iteration:** Save `nextNode` before removing current node
- **O(1) removal:** Hash map enables instant cancellation

---

### Pattern 3: Three Data Structures Combined

**Files:** [`orderbook.go`](./internal/orderbook/orderbook.go) + [`pricelevel.go`](./internal/orderbook/pricelevel.go) + [`rbtree.go`](./internal/orderbook/rbtree.go)

The order book achieves multiple performance goals by combining structures:

```
┌──────────────────────────────────────────────────┐
│ Order Book Architecture                          │
├──────────────────────────────────────────────────┤
│                                                  │
│  Hash Map (orders map[uint64]*OrderNode)        │
│  • Purpose: O(1) cancellation by order ID       │
│  • Usage: orders[12345] → direct pointer        │
│                                                  │
│        ↓ points to                               │
│                                                  │
│  Red-Black Tree (bids/asks *RBTree)             │
│  • Purpose: Sorted price levels                 │
│  • Complexity: O(log P) insert/delete           │
│  • Best price: O(1) via cached min pointer      │
│                                                  │
│        ↓ each level has                          │
│                                                  │
│  Doubly-Linked List (PriceLevel queue)          │
│  • Purpose: FIFO ordering at each price         │
│  • Complexity: O(1) append, O(1) remove         │
│                                                  │
└──────────────────────────────────────────────────┘
```

#### Adding an Order: `AddOrder()`

```go
AddOrder(order) {
    // 1. Find or create price level (RB-Tree)
    tree = getTree(order.Side)  // bids or asks
    level = tree.Get(order.Price)

    if level == nil {
        level = NewPriceLevel(order.Price)
        tree.Insert(level)  // O(log P) with auto-balancing
    }

    // 2. Append to FIFO queue (Doubly-Linked List)
    node = level.Append(order)  // O(1) - add to tail

    // 3. Track for fast cancellation (Hash Map)
    orders[order.ID] = node     // O(1)
}
```

**Time complexity:** O(log P) where P = number of price levels (typically 100-500)

#### Canceling an Order: `CancelOrder()`

```go
CancelOrder(orderID) {
    // 1. Direct lookup (Hash Map: O(1))
    node = orders[orderID]
    if node == nil { return nil }

    // 2. Remove from linked list (O(1))
    level = node.level
    level.Remove(node)  // Relink prev/next pointers

    // 3. Delete from hash map (O(1))
    delete(orders, orderID)

    // 4. Clean up empty level (RB-Tree: O(log P))
    if level.IsEmpty() {
        tree.Delete(level.Price)
    }

    return node.Order
}
```

**Time complexity:** O(1) amortized (tree deletion only if level becomes empty)

#### Getting Best Price: `GetBestBid/Ask()`

```go
GetBestBid() {
    return bids.Min()  // O(1) - cached pointer
}

GetBestAsk() {
    return asks.Min()  // O(1) - cached pointer
}
```

**Why O(1)?** RB-Tree maintains cached min/max pointers updated during insert/delete

---

### Pattern 4: Non-Blocking Market Data

**Function:** `PublishL1()` and `PublishTrade()` in [`internal/marketdata/publisher.go`](./internal/marketdata/publisher.go)

Market data uses non-blocking sends to prevent slow subscribers from blocking the matching engine:

```go
PublishTrade(trade) {
    for _, subscriberChan := range tradeSubs[trade.Symbol] {
        select {
        case subscriberChan <- trade:
            // Sent successfully
        default:
            // Channel full - subscriber is slow
            // DROP update instead of blocking
            atomic.AddInt64(&droppedUpdates, 1)
        }
    }
}
```

**Critical design decision:**
- ✅ Matching engine never blocks on slow subscribers
- ✅ Fast subscribers get all updates
- ⚠️ Slow subscribers miss updates (must reconnect & snapshot)

---

## Execution Example: Limit Order

### Scenario: Buy 100 AAPL @ $150.50

**Initial Order Book:**
```
ASKS (Sellers):
  $150.25: 50 shares  (Order #101 @ 09:55)
  $150.50: 100 shares (Order #102 @ 09:58)
  $151.00: 200 shares (Order #103 @ 10:01)

BIDS (Buyers):
  $150.00: 150 shares (Order #104 @ 09:56)
```

### Execution Trace

```
┌─────────────────────────────────────────────────┐
│ 1. HTTP POST /order                             │
│    JSON: {"symbol":"AAPL", "side":"buy",        │
│           "price":"150.50", "quantity":100}     │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 2. Gateway: handleOrder()                       │
│    • Parse JSON → OrderRequest                  │
│    • "150.50" → 15050 cents (int64)            │
│    • Create Order struct                        │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 3. Risk Check: riskChecker.Check()             │
│    ✓ Qty 100 < max 1000                        │
│    ✓ Value $15,050 < max $50,000               │
│    ✓ Price within 10% band                     │
│    → Passed                                     │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 4. Matching: engine.ProcessOrder()             │
│    • Assign order.ID = 105                     │
│    • Assign order.SequenceNum = 67890          │
│    • Call matchOrder()                         │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 5. Match Iteration #1                          │
│                                                 │
│ getMatchLevel() → book.GetBestAsk()            │
│   → Returns: PriceLevel @ $150.25              │
│                                                 │
│ priceAcceptable(15025)? → 15025 <= 15050 ✓    │
│                                                 │
│ level.Head() → Order #101 (50 shares)         │
│                                                 │
│ fillQty = min(100, 50) = 50                    │
│                                                 │
│ Create Fill:                                    │
│   • TradeID: 501                               │
│   • Price: $150.25 ← Maker's price!           │
│   • Quantity: 50                               │
│                                                 │
│ Update quantities:                              │
│   • Taker filled: 50/100                       │
│   • Maker filled: 50/50 (complete)             │
│                                                 │
│ book.CancelOrder(101) - Remove filled order    │
│   • orders[101] → node (O(1))                  │
│   • level.Remove(node) (O(1))                  │
│   • $150.25 level now empty                    │
│   • tree.Delete($150.25) (O(log P))            │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 6. Match Iteration #2                          │
│                                                 │
│ Taker remaining: 100 - 50 = 50                 │
│                                                 │
│ getMatchLevel() → book.GetBestAsk()            │
│   → Returns: PriceLevel @ $150.50              │
│                                                 │
│ priceAcceptable(15050)? → 15050 <= 15050 ✓    │
│                                                 │
│ level.Head() → Order #102 (100 shares)        │
│                                                 │
│ fillQty = min(50, 100) = 50                    │
│                                                 │
│ Create Fill:                                    │
│   • TradeID: 502                               │
│   • Price: $150.50                             │
│   • Quantity: 50                               │
│                                                 │
│ Update quantities:                              │
│   • Taker filled: 100/100 (complete!)          │
│   • Maker filled: 50/100 (partial)             │
│                                                 │
│ level.UpdateQuantity(-50)                      │
│   • TotalQty: 100 → 50                         │
│   • Order #102 stays in book with 50 remaining │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 7. Matching Complete                           │
│    order.RemainingQty() = 0 → EXIT             │
│    order.Status = OrderStatusFilled            │
│    Return fills: [Fill #501, Fill #502]        │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 8. Post-Processing                             │
│                                                 │
│ eventLog.Append(NewOrderEvent)                 │
│ eventLog.Append(FillEvent #501)                │
│ eventLog.Append(FillEvent #502)                │
│                                                 │
│ clearingHouse.RecordTrade(#501)                │
│ clearingHouse.RecordTrade(#502)                │
│                                                 │
│ riskChecker.UpdatePosition(TRADER1, +100)      │
│ riskChecker.SetReferencePrice(AAPL, 15050)     │
│                                                 │
│ publisher.PublishTrade(#501)                   │
│ publisher.PublishTrade(#502)                   │
│ publisher.PublishL1(bestBid/Ask updated)       │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 9. HTTP Response                                │
│    {                                            │
│      "success": true,                           │
│      "order_id": 105,                           │
│      "status": "filled",                        │
│      "filled_qty": 100,                         │
│      "fills": [                                 │
│        {"trade_id":501, "price":"150.25", "quantity":50},│
│        {"trade_id":502, "price":"150.50", "quantity":50} │
│      ]                                          │
│    }                                            │
└─────────────────────────────────────────────────┘
```

### Result Summary

**Final Order Book:**
```
ASKS (Sellers):
  $150.50: 50 shares  (Order #102, partially filled)
  $151.00: 200 shares (Order #103)

BIDS (Buyers):
  $150.00: 150 shares (Order #104)
```

**Key Observations:**
1. **Price Improvement:** Taker wanted $150.50, got 50 shares @ $150.25 (saved $0.25/share = $12.50)
2. **FIFO Enforced:** Order #101 (09:55) matched before Order #102 (09:58)
3. **Fixed-Point Math:** All prices as int64 cents (15025, 15050) - no rounding errors
4. **Efficient Operations:**
   - Best ask lookup: O(1)
   - FIFO queue walk: O(1) per order
   - Order removal: O(1) hash + O(1) linked list
   - Tree rebalance: O(log P) only when level empty

---

## Execution Example: Order Cancellation

### Scenario: Cancel Order #104 (Bid 150 @ $150.00)

```
┌─────────────────────────────────────────────────┐
│ 1. HTTP DELETE /cancel?symbol=AAPL&order_id=104│
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 2. Gateway: handleCancel()                      │
│    • Parse query params                         │
│    • symbol = "AAPL"                            │
│    • orderID = 104                              │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 3. Engine: CancelOrder("AAPL", 104)            │
│    book = orderBooks["AAPL"]                    │
│    order = book.CancelOrder(104)                │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 4. Order Book: CancelOrder(104)                │
│                                                 │
│ Step A: Hash lookup (O(1))                     │
│   node = orders[104]                            │
│   → Found: OrderNode{Order:#104, level:$150.00}│
│                                                 │
│ Step B: Remove from linked list (O(1))         │
│   level = node.level                            │
│   level.Remove(node)                            │
│     • node.prev.next = node.next                │
│     • node.next.prev = node.prev                │
│     • TotalQty -= 150 → 0                       │
│     • count-- → 0                               │
│                                                 │
│ Step C: Delete from hash map (O(1))            │
│   delete(orders, 104)                           │
│                                                 │
│ Step D: Remove empty level (O(log P))          │
│   if level.IsEmpty():                           │
│     tree.Delete($150.00)  // RB-Tree rebalance │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 5. Post-Processing                             │
│    order.Status = OrderStatusCancelled          │
│    eventLog.Append(OrderCancelledEvent)         │
└────────────────┬────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────────────┐
│ 6. HTTP Response                                │
│    {                                            │
│      "success": true,                           │
│      "order_id": 104,                           │
│      "cancelled_qty": 150                       │
│    }                                            │
└─────────────────────────────────────────────────┘
```

**Complexity Analysis:**
- Hash map lookup: O(1)
- Doubly-linked list removal: O(1)
- RB-Tree deletion: O(log P) where P ≈ 100-500
- **Total: O(1) amortized** (tree ops rare)

---

## Data Structure Deep Dive

### The Complete Order Book Structure

```
┌────────────────────────────────────────────────────────────┐
│                    OrderBook{symbol: "AAPL"}               │
├────────────────────────────────────────────────────────────┤
│                                                            │
│  ┌──────────────────────────────────────────────────┐     │
│  │ orders: map[uint64]*OrderNode                    │     │
│  │ ┌────────────────────────────────────────────┐   │     │
│  │ │ 101 → *OrderNode{Order:#101, level:$150.25}│  │     │
│  │ │ 102 → *OrderNode{Order:#102, level:$150.50}│  │     │
│  │ │ 103 → *OrderNode{Order:#103, level:$151.00}│  │     │
│  │ │ 104 → *OrderNode{Order:#104, level:$150.00}│  │     │
│  │ └────────────────────────────────────────────┘   │     │
│  │                                                  │     │
│  │ Purpose: O(1) cancellation by order ID          │     │
│  └──────────────────────────────────────────────────┘     │
│                                                            │
│  ┌──────────────────────────────────────────────────┐     │
│  │ bids: *RBTree (descending, highest first)       │     │
│  │                                                  │     │
│  │        Root: $150.00 [Black]                     │     │
│  │       /                     \                    │     │
│  │  $149.50 [Red]          $148.00 [Red]           │     │
│  │                                                  │     │
│  │ Traversal: $150.00 → $149.50 → $148.00 ↓        │     │
│  │ Cached min: → $150.00 (best bid)                │     │
│  │                                                  │     │
│  │ Purpose: Sorted price levels, O(1) best price   │     │
│  └──────────────────────────────────────────────────┘     │
│                                                            │
│  ┌──────────────────────────────────────────────────┐     │
│  │ asks: *RBTree (ascending, lowest first)         │     │
│  │                                                  │     │
│  │        Root: $150.50 [Black]                     │     │
│  │       /                     \                    │     │
│  │  $150.25 [Red]          $151.00 [Red]           │     │
│  │                                                  │     │
│  │ Traversal: $150.25 → $150.50 → $151.00 ↑        │     │
│  │ Cached min: → $150.25 (best ask)                │     │
│  │                                                  │     │
│  │ Purpose: Sorted price levels, O(1) best price   │     │
│  └──────────────────────────────────────────────────┘     │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

### Price Level Structure (FIFO Queue)

```
┌─────────────────────────────────────────────────────────┐
│ PriceLevel{Price: $150.00, TotalQty: 250, count: 3}    │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  head → [Order #106: 100sh @ 09:55] ← oldest (FIFO)    │
│            ↕                                            │
│         [Order #107: 50sh  @ 09:58]                     │
│            ↕                                            │
│  tail → [Order #108: 100sh @ 10:01] ← newest           │
│                                                         │
│  Doubly-Linked List:                                    │
│  • Append(): O(1) - add to tail                        │
│  • Remove(): O(1) - relink prev/next                   │
│  • Head():   O(1) - return head pointer                │
│                                                         │
│  Each OrderNode has:                                    │
│  • prev: *OrderNode  (for O(1) removal)                │
│  • next: *OrderNode  (for iteration)                   │
│  • level: *PriceLevel (back-pointer)                   │
│  • Order: *Order     (the actual order data)           │
└─────────────────────────────────────────────────────────┘
```

### Why This Design?

```
┌──────────────────────┬─────────────────┬──────────────────┐
│ Operation            │ Data Structure  │ Complexity       │
├──────────────────────┼─────────────────┼──────────────────┤
│ Get best bid/ask     │ RB-Tree cache   │ O(1)             │
│ Add order            │ RB-Tree insert  │ O(log P)         │
│                      │ + List append   │ + O(1)           │
│ Cancel by ID         │ Hash lookup     │ O(1)             │
│                      │ + List remove   │ + O(1)           │
│ Match (FIFO walk)    │ List iteration  │ O(M) fills       │
│ Get depth (top N)    │ RB-Tree walk    │ O(N)             │
└──────────────────────┴─────────────────┴──────────────────┘

Trade-offs:
✓ Fast best price (O(1) - critical for matching)
✓ Fast cancellation (O(1) - critical for active traders)
✓ FIFO guaranteed (linked list maintains insertion order)
✗ Memory overhead (3 pointers per order: prev, next, level)
✗ Cache misses (pointer chasing in linked list)

Alternative considered: Array-based FIFO
  ✓ Better cache locality
  ✗ O(n) removal from middle (unacceptable for cancels)
  ✗ Reallocation overhead
```

### Memory Layout Example

```
For an order book with 5 orders across 3 price levels:

Hash Map: ~80 bytes (5 entries × 16 bytes)
RB-Tree: ~240 bytes (3 levels × 80 bytes/node)
LinkedList Nodes: ~160 bytes (5 orders × 32 bytes/node)
Order structs: ~400 bytes (5 orders × 80 bytes)

Total: ~880 bytes for 5 orders

Extrapolating:
  1,000 orders: ~176 KB
  100,000 orders: ~17.6 MB
  1,000,000 orders: ~176 MB

Acceptable for modern systems (32+ GB RAM)
```

---

## Key Takeaways

### Design Patterns Used

1. **Single-Threaded Core (LMAX Disruptor)**
   - One mutex protects entire matching process
   - No lock contention in hot path
   - Deterministic execution (same input → same output)

2. **Hybrid Data Structures**
   - Combine hash map + tree + linked list
   - Each solves one specific problem
   - Overall complexity optimized for exchange workload

3. **Closure-Based Polymorphism**
   - One matching loop for buy/sell
   - Closures capture price logic
   - Cleaner than inheritance or switch statements

4. **Non-Blocking Pub/Sub**
   - Slow subscribers don't block engine
   - Dropped updates tracked but not retried
   - Matching engine always makes forward progress

5. **Event Sourcing**
   - All state changes logged
   - Enables replay and audit
   - Required for regulatory compliance

### Performance Characteristics

```
Operation               Time        Notes
─────────────────────────────────────────────────────────
Process order          ~0.7 µs      Full order → fills
Get best bid/ask       ~2 ns        Cached pointer
Add order to book      ~100 ns      Tree insert + list append
Cancel order           ~50 ns       Hash + list removal
Match single fill      ~200 ns      Includes fill creation
Event log append       ~500 ns      Buffered, batched sync

Throughput: ~1.5M orders/sec on commodity hardware
```

### Common Pitfalls Avoided

❌ **Don't use floats for money** → Use int64 cents (fixed-point)
❌ **Don't use locks in matching** → Single-threaded core
❌ **Don't block on I/O** → Buffered writes, non-blocking pub/sub
❌ **Don't allocate in hot path** → Pre-allocate, reuse objects
❌ **Don't traverse tree for best price** → Cache min/max pointers

---

## Next Steps

### Explore the Code

**Recommended reading order:**

1. **Start with:** [`internal/matching/engine.go`](./internal/matching/engine.go) - Core matching logic
   - Read `ProcessOrder()` to understand the main flow
   - Study `matchOrder()` to see FIFO matching in action

2. **Then read:** [`internal/orderbook/orderbook.go`](./internal/orderbook/orderbook.go) - Data structure
   - See how `AddOrder()` combines three data structures
   - Understand `CancelOrder()` O(1) complexity

3. **Understand:** [`internal/orderbook/pricelevel.go`](./internal/orderbook/pricelevel.go) - FIFO queue
   - Study `Append()` for adding orders to tail
   - Study `Remove()` for O(1) removal anywhere

4. **See HTTP layer:** [`cmd/server/main.go`](./cmd/server/main.go) - HTTP gateway
   - Study `handleOrder()` for request processing
   - See how components are orchestrated

5. **Run tests:** [`tests/integration_test.go`](./tests/integration_test.go) - Real scenarios
   - See complete end-to-end test cases
   - Understand verification patterns

### Modify the System

**Common modifications:**

| Goal | What to Change | Files to Edit |
|------|---------------|---------------|
| Add new order type (e.g., Stop-Loss) | Extend matching logic | [`engine.go`](./internal/matching/engine.go#L162) + [`types.go`](./internal/orders/types.go) |
| Change FIFO to Pro-Rata | Modify distribution algorithm | [`pricelevel.go`](./internal/orderbook/pricelevel.go#L65) |
| Add maker/taker fees | Track and calculate fees | [`engine.go`](./internal/matching/engine.go#L219) |
| Add market maker protections | Extend risk checks | [`checker.go`](./internal/risk/checker.go) |
| Optimize performance | Profile with `pprof` | Focus on hot paths in matching |
| Add WebSocket support | Real-time streaming | [`publisher.go`](./internal/marketdata/publisher.go) |

### Performance Profiling

```bash
# Run with profiling enabled
go run ./cmd/server -cpuprofile=cpu.prof

# Analyze profile
go tool pprof cpu.prof
> top10        # Show top 10 functions by CPU time
> list matchOrder  # Show source code with timing
```
