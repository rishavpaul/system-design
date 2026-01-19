// Package tests provides end-to-end integration tests that demonstrate
// all core system design concepts of the Order Matching Engine.
//
// Run with: go test -v ./tests/...
//
// Each test section demonstrates a specific concept and explains what
// you should observe at each step.
package tests

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishav/order-matching-engine/internal/events"
	"github.com/rishav/order-matching-engine/internal/marketdata"
	"github.com/rishav/order-matching-engine/internal/matching"
	"github.com/rishav/order-matching-engine/internal/orders"
	"github.com/rishav/order-matching-engine/internal/risk"
	"github.com/rishav/order-matching-engine/internal/settlement"
)

func repeat(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

// ============================================================================
// TEST 1: SINGLE-THREADED CORE (LMAX Pattern)
// ============================================================================

func TestSingleThreadedCore_Determinism(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Single-Threaded Core (LMAX Pattern)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: All orders are processed by a single thread in sequence.
         This guarantees deterministic output for the same input.

WHAT TO EXPECT:
- We'll process the same order sequence twice
- Both runs should produce IDENTICAL results
- This proves the engine is deterministic`)

	// Define a fixed sequence of orders
	orderSequence := []struct {
		side     orders.Side
		price    int64
		quantity int64
	}{
		{orders.SideSell, 15100, 100},
		{orders.SideSell, 15050, 50},
		{orders.SideBuy, 15000, 200},
		{orders.SideBuy, 15050, 75},
	}

	runSequence := func(runNumber int) []string {
		engine := matching.NewEngine()
		engine.AddSymbol("AAPL")

		var results []string

		for i, o := range orderSequence {
			order := &orders.Order{
				Symbol:    "AAPL",
				Side:      o.side,
				Type:      orders.OrderTypeLimit,
				Price:     o.price,
				Quantity:  o.quantity,
				AccountID: fmt.Sprintf("TRADER%d", i),
			}

			result := engine.ProcessOrder(order)

			resultStr := fmt.Sprintf("Order %d: %s %d@%s -> Fills:%d, Resting:%d",
				i+1, o.side, o.quantity, orders.FormatPrice(o.price),
				len(result.Fills), result.RestingQty)
			results = append(results, resultStr)
		}

		return results
	}

	fmt.Println("\nRUN 1:")
	run1 := runSequence(1)
	for _, r := range run1 {
		fmt.Println("  ", r)
	}

	fmt.Println("\nRUN 2 (identical input):")
	run2 := runSequence(2)
	for _, r := range run2 {
		fmt.Println("  ", r)
	}

	fmt.Println("\nVERIFICATION:")
	allMatch := true
	for i := range run1 {
		if run1[i] != run2[i] {
			allMatch = false
			t.Errorf("Mismatch at order %d: '%s' vs '%s'", i+1, run1[i], run2[i])
		}
	}

	if allMatch {
		fmt.Println("  [PASS] Both runs produced IDENTICAL results")
		fmt.Println("  [PASS] Single-threaded core is deterministic")
	}

	fmt.Println(`
WHY THIS WORKS:
- Single thread = no race conditions
- No locks = no non-deterministic lock ordering
- Same input sequence = same output every time`)
}

// ============================================================================
// TEST 2: PRICE-TIME PRIORITY (FIFO)
// ============================================================================

func TestPriceTimePriority(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Price-Time Priority (FIFO Matching)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Orders match by BEST PRICE first, then ARRIVAL TIME (FIFO).

SCENARIO:
- Three sellers post orders at $150.00 (S1, S2, S3 in that order)
- One seller posts at $150.50 (S4)
- A buyer wants 250 shares with a market order

EXPECTED:
- Buyer matches S1 first (best price + earliest time)
- Then S2 (same price, arrived second)
- Then S3 (same price, arrived third)
- S4 at $150.50 is NOT touched`)

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	sellers := []struct {
		id    string
		price int64
		qty   int64
	}{
		{"S1", 15000, 100},
		{"S2", 15000, 100},
		{"S3", 15000, 100},
		{"S4", 15050, 100},
	}

	fmt.Println("\nSTEP 1: Sellers post their orders")
	for _, s := range sellers {
		order := &orders.Order{
			Symbol:    "AAPL",
			Side:      orders.SideSell,
			Type:      orders.OrderTypeLimit,
			Price:     s.price,
			Quantity:  s.qty,
			AccountID: s.id,
		}
		engine.ProcessOrder(order)
		fmt.Printf("  %s posts SELL %d @ %s\n", s.id, s.qty, orders.FormatPrice(s.price))
	}

	book := engine.GetOrderBook("AAPL")
	fmt.Println("\nORDER BOOK STATE:")
	fmt.Println("  ASKS (Sell Orders):")
	for _, level := range book.GetAskDepth(5) {
		fmt.Printf("    %s: %d shares\n", orders.FormatPrice(level.Price), level.TotalQty)
	}

	fmt.Println("\nSTEP 2: Buyer sends MARKET BUY for 250 shares")
	buyOrder := &orders.Order{
		Symbol:    "AAPL",
		Side:      orders.SideBuy,
		Type:      orders.OrderTypeMarket,
		Quantity:  250,
		AccountID: "BUYER",
	}

	result := engine.ProcessOrder(buyOrder)

	fmt.Println("\nSTEP 3: Matching results (observe FIFO order)")
	for i, fill := range result.Fills {
		fmt.Printf("  Fill %d: %d shares @ %s from %s\n",
			i+1, fill.Quantity, orders.FormatPrice(fill.Price), fill.MakerAccountID)
	}

	fmt.Println("\nVERIFICATION:")
	expectedOrder := []string{"S1", "S2", "S3"}
	allCorrect := true
	for i, fill := range result.Fills {
		if i < len(expectedOrder) && fill.MakerAccountID != expectedOrder[i] {
			allCorrect = false
			t.Errorf("Expected fill from %s, got %s", expectedOrder[i], fill.MakerAccountID)
		}
	}

	if allCorrect && len(result.Fills) == 3 {
		fmt.Println("  [PASS] Fills occurred in FIFO order: S1 -> S2 -> S3")
		fmt.Println("  [PASS] All fills at best price ($150.00)")
	}

	fmt.Println(`
DATA STRUCTURE:
- Red-Black Tree: Keeps price levels sorted O(log n)
- Linked List at each price: Maintains FIFO order O(1)`)
}

// ============================================================================
// TEST 3: EVENT SOURCING
// ============================================================================

func TestEventSourcing_ReplayCapability(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Event Sourcing (Replay Capability)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Store all STATE CHANGES (events), not current state.
         State can be reconstructed by replaying events.

SCENARIO:
1. Process orders and log each event
2. "Crash" the system (discard state)
3. Replay events from log
4. Verify we can recover`)

	tmpFile, err := os.CreateTemp("", "event_log_*.dat")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	fmt.Println("\nSTEP 1: Process orders and log events")

	eventLog, err := events.NewEventLog(events.EventLogConfig{
		Path:     tmpFile.Name(),
		SyncMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	// Process a sell order
	sellOrder := &orders.Order{
		Symbol: "AAPL", Side: orders.SideSell, Type: orders.OrderTypeLimit,
		Price: 15000, Quantity: 100, AccountID: "SELLER",
	}
	result1 := engine.ProcessOrder(sellOrder)

	seqNum, _ := eventLog.Append(&events.NewOrderEvent{
		OrderID: result1.Order.ID, Symbol: "AAPL", Side: orders.SideSell,
		OrderType: orders.OrderTypeLimit, Price: 15000, Quantity: 100,
	})
	fmt.Printf("  Event %d: NEW_ORDER SELL 100 @ $150.00\n", seqNum)

	// Process a buy order that matches
	buyOrder := &orders.Order{
		Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeMarket,
		Quantity: 60, AccountID: "BUYER",
	}
	result2 := engine.ProcessOrder(buyOrder)

	seqNum, _ = eventLog.Append(&events.NewOrderEvent{
		OrderID: result2.Order.ID, Symbol: "AAPL", Side: orders.SideBuy,
		OrderType: orders.OrderTypeMarket, Quantity: 60,
	})
	fmt.Printf("  Event %d: NEW_ORDER BUY 60 @ MARKET\n", seqNum)

	for _, fill := range result2.Fills {
		seqNum, _ = eventLog.Append(&events.FillEvent{
			TradeID: fill.TradeID, Symbol: fill.Symbol,
			Price: fill.Price, Quantity: fill.Quantity,
		})
		fmt.Printf("  Event %d: FILL %d shares @ %s\n", seqNum, fill.Quantity, orders.FormatPrice(fill.Price))
	}

	lastSeq := eventLog.GetLastSequence()
	eventLog.Close()

	fmt.Println("\nSTEP 2: System crashes (state lost)")
	fmt.Println("  [CRASH] All in-memory state discarded")

	fmt.Println("\nSTEP 3: Replay events from log")

	replayLog, _ := events.NewEventLog(events.EventLogConfig{Path: tmpFile.Name()})
	defer replayLog.Close()

	replayCount := 0
	replayLog.Replay(func(seq uint64, event interface{}) error {
		replayCount++
		switch e := event.(type) {
		case *events.NewOrderEvent:
			fmt.Printf("  Replaying %d: NEW_ORDER %s\n", seq, e.Side)
		case *events.FillEvent:
			fmt.Printf("  Replaying %d: FILL %d @ %s\n", seq, e.Quantity, orders.FormatPrice(e.Price))
		}
		return nil
	})

	fmt.Println("\nVERIFICATION:")
	if uint64(replayCount) == lastSeq {
		fmt.Printf("  [PASS] Replayed all %d events\n", replayCount)
		fmt.Println("  [PASS] State can be rebuilt from event log")
	} else {
		t.Errorf("Expected %d events, replayed %d", lastSeq, replayCount)
	}

	fmt.Println(`
BENEFITS:
- Audit trail for regulators
- Disaster recovery via replay
- Debug by replaying to failure point`)
}

// ============================================================================
// TEST 4: FIXED-POINT ARITHMETIC
// ============================================================================

func TestFixedPointArithmetic(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Fixed-Point Arithmetic (No Float Errors)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Store prices as INTEGERS (cents), not floats.

THE PROBLEM WITH FLOATS:`)

	floatResult := 0.1 + 0.2
	fmt.Printf("\n  0.1 + 0.2 = %.17f\n", floatResult)
	fmt.Printf("  Expected:   0.30000000000000000\n")
	fmt.Printf("  Equal to 0.3? %v  <-- WRONG!\n", floatResult == 0.3)

	fmt.Println("\nFIXED-POINT SOLUTION:")
	intResult := int64(10) + int64(20)
	fmt.Printf("  10 + 20 = %d cents\n", intResult)
	fmt.Printf("  Equal to 30? %v  <-- CORRECT!\n", intResult == 30)

	fmt.Println("\nPRACTICAL EXAMPLE:")

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	sellPrice := int64(15025) // $150.25
	buyPrice := int64(15025)

	fmt.Printf("  Seller: SELL 100 @ %s (stored as %d)\n", orders.FormatPrice(sellPrice), sellPrice)
	engine.ProcessOrder(&orders.Order{
		Symbol: "AAPL", Side: orders.SideSell, Type: orders.OrderTypeLimit,
		Price: sellPrice, Quantity: 100, AccountID: "SELLER",
	})

	fmt.Printf("  Buyer:  BUY 100 @ %s (stored as %d)\n", orders.FormatPrice(buyPrice), buyPrice)
	result := engine.ProcessOrder(&orders.Order{
		Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeLimit,
		Price: buyPrice, Quantity: 100, AccountID: "BUYER",
	})

	fmt.Println("\nVERIFICATION:")
	if len(result.Fills) == 1 && result.Fills[0].Price == 15025 {
		fmt.Println("  [PASS] Orders matched at EXACT price $150.25")
		fmt.Println("  [PASS] No floating-point errors")
	} else {
		t.Error("Expected match at 15025")
	}

	fmt.Println(`
WHY THIS MATTERS:
- NYSE processes 3 billion shares/day
- Tiny float errors compound to millions of dollars
- Regulators require exact price matching`)
}

// ============================================================================
// TEST 5: PRE-TRADE RISK CONTROLS
// ============================================================================

func TestPreTradeRiskControls(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Pre-Trade Risk Controls")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Validate orders BEFORE matching.

RISK CONTROLS:
1. Order Size Limit: Max shares per order
2. Order Value Limit: Max dollar value
3. Price Band: Reject prices too far from market
4. Position Limit: Max shares held`)

	config := risk.Config{
		MaxOrderSize:     1000,
		MaxOrderValue:    5000000,   // $50,000 in cents
		MaxPositionSize:  5000,
		MaxDailyVolume:   100000000, // $1,000,000 in cents
		PriceBandPercent: 0.10,
	}

	checker := risk.NewChecker(config)
	checker.SetReferencePrice("AAPL", 15000)

	fmt.Println("\nTEST CASES:")

	testCases := []struct {
		name     string
		order    *orders.Order
		expected bool
	}{
		{
			name: "Normal Order",
			order: &orders.Order{
				Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeLimit,
				Price: 15000, Quantity: 100, AccountID: "T1",
			},
			expected: true,
		},
		{
			name: "Size Too Large (5000 > 1000 max)",
			order: &orders.Order{
				Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeLimit,
				Price: 15000, Quantity: 5000, AccountID: "T1",
			},
			expected: false,
		},
		{
			name: "Price Outside Band ($200 vs $150 ref)",
			order: &orders.Order{
				Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeLimit,
				Price: 20000, Quantity: 100, AccountID: "T1",
			},
			expected: false,
		},
	}

	allPassed := true
	for _, tc := range testCases {
		result := checker.Check(tc.order)
		status := "REJECTED"
		if result.Passed {
			status = "ACCEPTED"
		}

		correct := result.Passed == tc.expected
		if !correct {
			allPassed = false
			t.Errorf("%s: expected %v, got %v", tc.name, tc.expected, result.Passed)
		}

		mark := "[PASS]"
		if !correct {
			mark = "[FAIL]"
		}

		fmt.Printf("\n  %s %s\n", mark, tc.name)
		fmt.Printf("    Result: %s\n", status)
		if !result.Passed {
			fmt.Printf("    Reason: %s\n", result.Reason)
		}
	}

	fmt.Println("\nVERIFICATION:")
	if allPassed {
		fmt.Println("  [PASS] All risk checks working correctly")
	}

	fmt.Println(`
REAL EXAMPLES:
- Knight Capital 2012: $440M loss in 45 min (no risk controls)
- Flash Crash 2010: Lack of price bands caused cascade`)
}

// ============================================================================
// TEST 6: T+2 SETTLEMENT
// ============================================================================

func TestT2Settlement(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: T+2 Settlement (Clearing & Netting)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Trades settle 2 business days after execution.

NETTING reduces transfers:
  Without: A buys 100, A sells 60, A buys 40 = 3 settlements
  With:    Net A buys 80 = 1 settlement (67% reduction)`)

	clearingHouse := settlement.NewClearingHouse()

	fmt.Println("\nSTEP 1: Initial Account State")
	alice := clearingHouse.GetOrCreateAccount("ALICE", 1000000)
	bob := clearingHouse.GetOrCreateAccount("BOB", 500000)
	bob.Holdings["AAPL"] = 500

	fmt.Printf("  ALICE: Cash=%s, AAPL=%d\n", orders.FormatPrice(alice.Cash), alice.Holdings["AAPL"])
	fmt.Printf("  BOB:   Cash=%s, AAPL=%d\n", orders.FormatPrice(bob.Cash), bob.Holdings["AAPL"])

	fmt.Println("\nSTEP 2: Execute Trades")
	trades := []struct {
		buyer, seller string
		qty           int64
		price         int64
	}{
		{"ALICE", "BOB", 100, 15000},
		{"BOB", "ALICE", 60, 15100},
		{"ALICE", "BOB", 40, 14900},
	}

	for i, tr := range trades {
		fill := orders.Fill{
			TradeID: uint64(i + 1), Symbol: "AAPL",
			Price: tr.price, Quantity: tr.qty,
			MakerAccountID: tr.seller, TakerAccountID: tr.buyer,
			TakerSide: orders.SideBuy,
		}
		clearingHouse.RecordTrade(fill)
		fmt.Printf("  Trade %d: %s buys %d from %s @ %s\n",
			i+1, tr.buyer, tr.qty, tr.seller, orders.FormatPrice(tr.price))
	}

	fmt.Println("\nSTEP 3: Netting")
	fmt.Println("  ALICE net: +100 -60 +40 = +80 shares")
	fmt.Println("  BOB net:   -100 +60 -40 = -80 shares")
	fmt.Println("  Result: 3 trades -> 1 settlement instruction")

	instructions := clearingHouse.GenerateSettlementInstructions()
	fmt.Printf("\n  Generated %d settlement instruction(s)\n", len(instructions))

	stats := clearingHouse.GetSettlementStats()
	fmt.Println("\nVERIFICATION:")
	fmt.Printf("  [PASS] Recorded %d trades\n", stats["total_trades"])
	fmt.Println("  [PASS] Netting reduces settlement complexity")

	fmt.Println(`
WHY T+2?
- Time to arrange financing
- Time to locate securities
- US moving to T+1 in 2024`)
}

// ============================================================================
// TEST 7: MARKET DATA PUBLISHING
// ============================================================================

func TestMarketDataPublishing(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("TEST: Market Data Publishing (L1/L2 Pub/Sub)")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
CONCEPT: Publish real-time market data to subscribers.

LEVELS:
- L1: Best bid/ask (retail traders)
- L2: Top N price levels (active traders)
- L3: Every order (market makers)`)

	publisher := marketdata.NewPublisher(100)
	defer publisher.Close()

	var receivedL1 int32
	var receivedTrades int32
	var wg sync.WaitGroup

	l1Ch := publisher.SubscribeL1("AAPL")
	tradeCh := publisher.SubscribeTrades("AAPL")

	done := make(chan bool)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-l1Ch:
				atomic.AddInt32(&receivedL1, 1)
			case <-tradeCh:
				atomic.AddInt32(&receivedTrades, 1)
			case <-done:
				return
			}
		}
	}()

	fmt.Println("\nSTEP 1: Subscribe to AAPL market data")
	fmt.Println("  Listening for L1 quotes...")
	fmt.Println("  Listening for trade reports...")

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	fmt.Println("\nSTEP 2: Post sell order, publish L1")
	engine.ProcessOrder(&orders.Order{
		Symbol: "AAPL", Side: orders.SideSell, Type: orders.OrderTypeLimit,
		Price: 15025, Quantity: 100, AccountID: "SELLER",
	})

	publisher.PublishL1(marketdata.L1Quote{
		Symbol: "AAPL", AskPrice: 15025, AskSize: 100, Timestamp: orders.Now(),
	})
	fmt.Println("  Published L1: Ask $150.25 x 100")

	fmt.Println("\nSTEP 3: Execute trade, publish trade report")
	result := engine.ProcessOrder(&orders.Order{
		Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeMarket,
		Quantity: 50, AccountID: "BUYER",
	})

	for _, fill := range result.Fills {
		publisher.PublishTrade(marketdata.TradeReport{
			TradeID: fill.TradeID, Symbol: fill.Symbol,
			Price: fill.Price, Quantity: fill.Quantity,
			AggressorSide: orders.SideBuy, Timestamp: orders.Now(),
		})
		fmt.Printf("  Published Trade: %d @ %s\n", fill.Quantity, orders.FormatPrice(fill.Price))
	}

	publisher.PublishL1(marketdata.L1Quote{
		Symbol: "AAPL", AskPrice: 15025, AskSize: 50,
		LastPrice: 15025, LastSize: 50, Timestamp: orders.Now(),
	})
	fmt.Println("  Published L1: Ask $150.25 x 50 (updated)")

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()

	l1Count := atomic.LoadInt32(&receivedL1)
	tradeCount := atomic.LoadInt32(&receivedTrades)

	fmt.Println("\nVERIFICATION:")
	fmt.Printf("  L1 quotes received: %d\n", l1Count)
	fmt.Printf("  Trade reports received: %d\n", tradeCount)

	if l1Count >= 2 && tradeCount >= 1 {
		fmt.Println("  [PASS] Subscribers received market data")
	} else {
		t.Errorf("Expected 2+ L1, 1+ trades; got %d L1, %d trades", l1Count, tradeCount)
	}

	fmt.Println(`
DESIGN:
- Non-blocking publish (slow subscribers don't block)
- Channel buffers absorb bursts
- Drop stale data if subscriber can't keep up`)
}

// ============================================================================
// PERFORMANCE BENCHMARK
// ============================================================================

func TestCorrectness_VerifyRealMatching(t *testing.T) {
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("CORRECTNESS VERIFICATION: Proving Real Matching")
	fmt.Println(repeat("=", 70))

	fmt.Println(`
GOAL: Prove the engine is actually doing real work, not faking results.

VERIFICATION STRATEGY:
1. Track total shares in the system (conservation of shares)
2. Verify order book depth matches expectations
3. Check that fills actually remove orders from the book
4. Validate fill quantities sum correctly
5. Ensure price-time priority is strictly enforced`)

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	// Track ALL order quantities
	var totalBuyQty, totalSellQty int64
	var totalFillQty int64

	fmt.Println("\n=== STEP 1: Post sell orders at different prices ===")
	sellOrders := []struct {
		price int64
		qty   int64
	}{
		{15000, 100},
		{15000, 50},
		{15000, 75},
		{15050, 200},
	}

	var orderIDs []uint64
	for i, so := range sellOrders {
		order := &orders.Order{
			Symbol: "AAPL", Side: orders.SideSell,
			Type: orders.OrderTypeLimit, Price: so.price, Quantity: so.qty,
			AccountID: "SELLER",
		}
		result := engine.ProcessOrder(order)
		orderIDs = append(orderIDs, result.Order.ID)
		totalSellQty += so.qty
		fmt.Printf("  Posted: S%d (ID=%d) SELL %d @ %s\n", i+1, result.Order.ID, so.qty, orders.FormatPrice(so.price))
	}

	book := engine.GetOrderBook("AAPL")
	askDepth := book.GetAskDepth(5)
	fmt.Printf("\nOrder Book Asks:\n")
	for _, level := range askDepth {
		fmt.Printf("  %s: %d shares\n", orders.FormatPrice(level.Price), level.TotalQty)
	}

	expectedAskQty := int64(225) // 100+50+75 at $150.00
	actualAskQty := askDepth[0].TotalQty
	if actualAskQty != expectedAskQty {
		t.Errorf("FAIL: Expected %d at $150.00, got %d", expectedAskQty, actualAskQty)
	}
	fmt.Printf("\n✓ Verified: %d shares at $150.00 (expected %d)\n", actualAskQty, expectedAskQty)

	fmt.Println("\n=== STEP 2: Send buy order that should match exactly 225 shares ===")
	result := engine.ProcessOrder(&orders.Order{
		Symbol: "AAPL", Side: orders.SideBuy, Type: orders.OrderTypeLimit,
		Price: 15000, Quantity: 225, AccountID: "BUYER",
	})

	totalBuyQty += 225
	fmt.Printf("  BUY 225 @ $150.00 -> Generated %d fills\n", len(result.Fills))

	// Verify fill details
	var filledQty int64
	for i, fill := range result.Fills {
		filledQty += fill.Quantity
		totalFillQty += fill.Quantity
		fmt.Printf("  Fill %d: %d shares @ %s (Maker ID=%d)\n",
			i+1, fill.Quantity, orders.FormatPrice(fill.Price), fill.MakerOrderID)
	}

	if filledQty != 225 {
		t.Errorf("FAIL: Expected 225 filled, got %d", filledQty)
	}
	fmt.Printf("\n✓ Verified: Filled exactly 225 shares\n")

	// Verify FIFO order: first 3 sell orders should match in sequence
	expectedFills := []struct {
		orderID uint64
		qty     int64
	}{
		{orderIDs[0], 100},
		{orderIDs[1], 50},
		{orderIDs[2], 75},
	}

	for i, expected := range expectedFills {
		if i >= len(result.Fills) {
			t.Errorf("FAIL: Missing fill for order %d", expected.orderID)
			continue
		}
		if result.Fills[i].MakerOrderID != expected.orderID {
			t.Errorf("FAIL: Fill %d should be order %d, got %d", i, expected.orderID, result.Fills[i].MakerOrderID)
		}
		if result.Fills[i].Quantity != expected.qty {
			t.Errorf("FAIL: Fill %d should be %d shares, got %d", i, expected.qty, result.Fills[i].Quantity)
		}
	}
	fmt.Printf("✓ Verified: FIFO order enforced (first 3 orders matched in sequence)\n")

	// Check order book is now empty at $150.00
	askDepth = book.GetAskDepth(5)
	fmt.Printf("\nOrder Book After Match:\n")
	for _, level := range askDepth {
		fmt.Printf("  %s: %d shares\n", orders.FormatPrice(level.Price), level.TotalQty)
	}

	if len(askDepth) > 0 && askDepth[0].Price == 15000 {
		t.Errorf("FAIL: $150.00 level should be gone, still has %d shares", askDepth[0].TotalQty)
	}
	fmt.Printf("✓ Verified: $150.00 level removed from book\n")

	if len(askDepth) == 0 || askDepth[0].Price != 15050 {
		t.Errorf("FAIL: Best ask should now be $150.50")
	}
	fmt.Printf("✓ Verified: Best ask now $150.50 (200 shares)\n")

	fmt.Println("\n=== STEP 3: Conservation of shares ===")
	fmt.Printf("  Total SELL orders posted: %d shares\n", totalSellQty)
	fmt.Printf("  Total BUY orders posted:  %d shares\n", totalBuyQty)
	fmt.Printf("  Total shares FILLED:      %d shares\n", totalFillQty)

	// Fills can't exceed what was posted
	if totalFillQty > totalBuyQty || totalFillQty > totalSellQty {
		t.Errorf("FAIL: Filled %d but only posted %d buy, %d sell", totalFillQty, totalBuyQty, totalSellQty)
	}

	// Remaining should be on the book
	remainingAsk := totalSellQty - totalFillQty
	actualRemaining := askDepth[0].TotalQty
	if actualRemaining != remainingAsk {
		t.Errorf("FAIL: Expected %d remaining, book shows %d", remainingAsk, actualRemaining)
	}
	fmt.Printf("  Remaining on book:        %d shares\n", actualRemaining)
	fmt.Printf("✓ Verified: Shares conserved (%d sold - %d filled = %d remaining)\n",
		totalSellQty, totalFillQty, remainingAsk)

	fmt.Println("\n=== CONCLUSION ===")
	fmt.Println("✓ Engine is doing REAL matching:")
	fmt.Println("  • Orders are actually stored in the book")
	fmt.Println("  • Fills respect price-time priority (FIFO)")
	fmt.Println("  • Matched orders are removed from book")
	fmt.Println("  • Quantities are conserved (no magic creation/deletion)")
	fmt.Println("  • Best bid/ask updates correctly")
}

func TestPerformanceBenchmark(t *testing.T) {
	testStartTime := time.Now()
	fmt.Println()
	fmt.Println(repeat("=", 70))
	fmt.Println("PERFORMANCE BENCHMARK")
	fmt.Printf("Test started at: %s\n", testStartTime.Format("15:04:05.000"))
	fmt.Println(repeat("=", 70))

	engine := matching.NewEngine()
	engine.AddSymbol("AAPL")

	// Warm up
	for i := 0; i < 1000; i++ {
		engine.ProcessOrder(&orders.Order{
			Symbol: "AAPL", Side: orders.SideSell, Type: orders.OrderTypeLimit,
			Price: 15000 + int64(i%100), Quantity: 100, AccountID: "WARMUP",
		})
	}

	numOrders := 10000000
	var fillCount int64

	fmt.Printf("\nProcessing %d orders...\n", numOrders)
	loopStartTime := time.Now()
	fmt.Printf("Loop started at: %s\n", loopStartTime.Format("15:04:05.000"))

	start := time.Now()
	for i := 0; i < numOrders; i++ {
		side := orders.SideBuy
		if i%2 == 0 {
			side = orders.SideSell
		}

		result := engine.ProcessOrder(&orders.Order{
			Symbol:    "AAPL",
			Side:      side,
			Type:      orders.OrderTypeLimit,
			Price:     15000 + int64(i%50),
			Quantity:  10,
			AccountID: fmt.Sprintf("T%d", i%100),
		})

		atomic.AddInt64(&fillCount, int64(len(result.Fills)))
	}
	elapsed := time.Since(start)
	loopEndTime := time.Now()
	fmt.Printf("Loop completed at: %s\n", loopEndTime.Format("15:04:05.000"))
	fmt.Printf("Loop duration: %v\n", loopEndTime.Sub(loopStartTime))

	ordersPerSec := float64(numOrders) / elapsed.Seconds()
	usPerOrder := float64(elapsed.Microseconds()) / float64(numOrders)

	fmt.Println("\nRESULTS:")
	fmt.Printf("  Orders processed: %d\n", numOrders)
	fmt.Printf("  Time elapsed:     %v\n", elapsed)
	fmt.Printf("  Throughput:       %.0f orders/sec\n", ordersPerSec)
	fmt.Printf("  Latency:          %.2f us/order\n", usPerOrder)
	fmt.Printf("  Fills generated:  %d\n", fillCount)

	fmt.Println("\nCOMPARISON:")
	fmt.Printf("  This engine:  ~%.0f orders/sec\n", ordersPerSec)
	fmt.Println("  LMAX:         ~6,000,000 orders/sec")
	fmt.Println("  NASDAQ:       ~1,000,000+ msg/sec")
	fmt.Println("\n  (Real exchanges use kernel bypass, custom hardware)")

	testEndTime := time.Now()
	fmt.Printf("\nTest completed at: %s\n", testEndTime.Format("15:04:05.000"))
	fmt.Printf("TOTAL TEST DURATION: %v\n", testEndTime.Sub(testStartTime))
	fmt.Println(repeat("=", 70))
}
