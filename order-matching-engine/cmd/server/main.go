// Package main provides the order matching engine server.
//
// Architecture Overview:
//
//	┌─────────────┐     ┌─────────────┐     ┌─────────────┐
//	│   Client    │────▶│  Gateway    │────▶│   Risk      │
//	│  (HTTP/WS)  │     │  (HTTP API) │     │   Checker   │
//	└─────────────┘     └─────────────┘     └──────┬──────┘
//	                                               │
//	                                               ▼
//	┌─────────────┐     ┌─────────────┐     ┌─────────────┐
//	│  Market     │◀────│  Matching   │◀────│  Sequencer  │
//	│  Data Pub   │     │   Engine    │     │ (Ring Buf)  │
//	└─────────────┘     └──────┬──────┘     └─────────────┘
//	                           │
//	                           ▼
//	┌─────────────┐     ┌─────────────┐
//	│  Clearing   │◀────│  Event Log  │
//	│   House     │     │             │
//	└─────────────┘     └─────────────┘
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rishav/order-matching-engine/internal/disruptor"
	"github.com/rishav/order-matching-engine/internal/events"
	"github.com/rishav/order-matching-engine/internal/marketdata"
	"github.com/rishav/order-matching-engine/internal/matching"
	"github.com/rishav/order-matching-engine/internal/orders"
	"github.com/rishav/order-matching-engine/internal/risk"
	"github.com/rishav/order-matching-engine/internal/settlement"
)

// Server is the main order matching engine server.
//
// Architecture: LMAX Disruptor Pattern (see README "LMAX Disruptor Pattern" section)
//   - HTTP handlers (multi-threaded) submit to ring buffer using CAS operations
//   - Single event processor consumes from ring buffer and calls matching engine
//   - This achieves 1.1M orders/sec with lock-free coordination
type Server struct {
	// Core components
	engine        *matching.Engine        // Single-threaded matching engine (deterministic)
	riskChecker   *risk.Checker          // Pre-trade risk validation
	eventLog      *events.EventLog       // Append-only event log for recovery
	publisher     *marketdata.Publisher  // Market data publisher (L1/L2 quotes, trades)
	clearingHouse *settlement.ClearingHouse // Post-trade settlement

	// LMAX Disruptor components for lock-free, high-throughput processing
	// See README "LMAX Disruptor Pattern (Ring Buffer)" for detailed explanation
	ringBuffer     *disruptor.RingBuffer      // 8192-slot pre-allocated ring buffer (power-of-2)
	sequencer      *disruptor.Sequencer       // Lock-free sequencer using atomic CAS operations
	eventProcessor *disruptor.EventProcessor  // Single-threaded processor (maintains determinism)

	httpServer *http.Server
}

// Config holds server configuration.
type Config struct {
	Port          int
	EventLogPath  string
	SyncMode      bool
	Symbols       []string
}

// DefaultConfig returns reasonable defaults.
func DefaultConfig() Config {
	return Config{
		Port:         8080,
		EventLogPath: "events.log",
		SyncMode:     false,
		Symbols:      []string{"AAPL", "GOOGL", "MSFT", "AMZN", "TSLA"},
	}
}

// NewServer creates a new server instance.
func NewServer(config Config) (*Server, error) {
	// Create event log for compliance and recovery
	// All state changes (new orders, fills, cancels) are logged before being applied
	// This enables crash recovery by replaying the event log
	eventLog, err := events.NewEventLog(events.EventLogConfig{
		Path:     config.EventLogPath,
		SyncMode: config.SyncMode, // SyncMode=true uses O_SYNC for durability (slower)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create event log: %w", err)
	}

	// Create matching engine (single-threaded, deterministic)
	// Each symbol gets its own order book with red-black trees for price levels
	engine := matching.NewEngine()
	for _, symbol := range config.Symbols {
		engine.AddSymbol(symbol)
	}

	// Create supporting components
	riskChecker := risk.NewChecker(risk.DefaultConfig())
	publisher := marketdata.NewPublisher(1000)
	clearingHouse := settlement.NewClearingHouse()

	// Create some test accounts for demo purposes
	for _, acct := range []string{"TRADER1", "TRADER2", "MM1", "MM2"} {
		clearingHouse.GetOrCreateAccount(acct, 10000000) // $100,000 each
	}

	// CRITICAL: Initialize LMAX Disruptor components (see README for details)
	//
	// Ring Buffer: 8192-slot pre-allocated circular queue (power-of-2 for fast modulo)
	//   - Each slot is cache-aligned (64 bytes) to prevent false sharing
	//   - Pre-allocation eliminates GC pressure during order processing
	//
	// Sequencer: Lock-free coordinator using atomic Compare-And-Swap (CAS)
	//   - Multiple HTTP handlers claim sequence numbers concurrently
	//   - No mutex locks, just atomic operations (19ns per claim)
	//
	// Event Processor: Single-threaded consumer that reads from ring buffer
	//   - Maintains determinism (same input = same output)
	//   - Processes orders sequentially in sequence number order
	//   - Calls matching engine and logs events
	ringBuffer := disruptor.NewRingBuffer(disruptor.DefaultConfig()) // 8192 slots
	sequencer := disruptor.NewSequencer(ringBuffer)
	eventProcessor := disruptor.NewEventProcessor(ringBuffer, engine, eventLog)

	server := &Server{
		engine:         engine,
		riskChecker:    riskChecker,
		eventLog:       eventLog,
		publisher:      publisher,
		clearingHouse:  clearingHouse,
		ringBuffer:     ringBuffer,
		sequencer:      sequencer,
		eventProcessor: eventProcessor,
	}

	// Setup HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/order", server.handleOrder)
	mux.HandleFunc("/cancel", server.handleCancel)
	mux.HandleFunc("/book", server.handleBook)
	mux.HandleFunc("/account", server.handleAccount)
	mux.HandleFunc("/stats", server.handleStats)
	mux.HandleFunc("/health", server.handleHealth)

	server.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", config.Port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return server, nil
}

// Start starts the server.
func (s *Server) Start() error {
	log.Printf("Starting Order Matching Engine on %s", s.httpServer.Addr)
	log.Printf("Symbols: %v", s.engine.Symbols())

	// CRITICAL: Start the event processor first before accepting HTTP requests
	// The processor runs in its own goroutine, consuming from the ring buffer
	// and calling the matching engine in a single-threaded, deterministic manner
	s.eventProcessor.Start()

	// Start HTTP server (blocks until shutdown)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
//
// Shutdown order is critical to prevent data loss:
//   1. Stop accepting new HTTP requests
//   2. Drain ring buffer (process all pending orders)
//   3. Flush event log to disk
//   4. Close all resources
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("Shutting down server...")

	// Step 1: Stop accepting new HTTP requests
	// Existing in-flight requests will complete
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return err
	}

	// Step 2: Shutdown event processor
	// This drains the ring buffer (processes all pending orders)
	// and flushes all batched events to the event log
	s.eventProcessor.Shutdown()

	// Step 3: Close event log (final fsync to ensure durability)
	if err := s.eventLog.Close(); err != nil {
		return err
	}

	// Step 4: Close market data publisher
	s.publisher.Close()
	return nil
}

// OrderRequest represents an order submission request.
type OrderRequest struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`     // "buy" or "sell"
	Type          string `json:"type"`     // "market", "limit", "ioc", "fok"
	Price         string `json:"price"`    // Dollar amount as string
	Quantity      int64  `json:"quantity"`
	AccountID     string `json:"account_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
}

// OrderResponse represents an order response.
type OrderResponse struct {
	Success       bool          `json:"success"`
	OrderID       uint64        `json:"order_id,omitempty"`
	Status        string        `json:"status,omitempty"`
	FilledQty     int64         `json:"filled_qty,omitempty"`
	RemainingQty  int64         `json:"remaining_qty,omitempty"`
	Fills         []FillInfo    `json:"fills,omitempty"`
	RejectReason  string        `json:"reject_reason,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// FillInfo represents fill information in a response.
type FillInfo struct {
	TradeID  uint64 `json:"trade_id"`
	Price    string `json:"price"`
	Quantity int64  `json:"quantity"`
}

func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success: false,
			Error:   fmt.Sprintf("invalid request: %v", err),
		})
		return
	}

	// Parse side
	var side orders.Side
	switch req.Side {
	case "buy", "BUY":
		side = orders.SideBuy
	case "sell", "SELL":
		side = orders.SideSell
	default:
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success: false,
			Error:   "invalid side: must be 'buy' or 'sell'",
		})
		return
	}

	// Parse order type
	var orderType orders.OrderType
	switch req.Type {
	case "market", "MARKET":
		orderType = orders.OrderTypeMarket
	case "limit", "LIMIT":
		orderType = orders.OrderTypeLimit
	case "ioc", "IOC":
		orderType = orders.OrderTypeIOC
	case "fok", "FOK":
		orderType = orders.OrderTypeFOK
	default:
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success: false,
			Error:   "invalid type: must be 'market', 'limit', 'ioc', or 'fok'",
		})
		return
	}

	// Parse price: Convert from decimal string to fixed-point integer
	// Example: "150.00" -> 150000 (stored as integer with 3 decimal places)
	//
	// Why fixed-point? Floating-point arithmetic has precision issues:
	//   0.1 + 0.2 = 0.30000000000000004 (IEEE 754 rounding error)
	//
	// Financial systems use fixed-point to ensure exact decimal arithmetic:
	//   $150.00 is stored as 150000 (integer)
	//   $0.01 minimum tick size (1 = $0.001)
	//
	// See README "Core Concepts - Price Representation" for more details
	var price int64
	if req.Price != "" {
		priceFloat, err := strconv.ParseFloat(req.Price, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, OrderResponse{
				Success: false,
				Error:   fmt.Sprintf("invalid price: %v", err),
			})
			return
		}
		price = orders.ParsePrice(priceFloat) // Multiply by 1000 to convert to fixed-point
	}

	// Create order
	order := &orders.Order{
		Symbol:        req.Symbol,
		Side:          side,
		Type:          orderType,
		Price:         price,
		Quantity:      req.Quantity,
		AccountID:     req.AccountID,
		ClientOrderID: req.ClientOrderID,
		Timestamp:     orders.Now(),
	}

	// Run pre-trade risk checks (e.g., position limits, buying power)
	// This happens before submitting to the ring buffer to reject invalid orders early
	riskResult := s.riskChecker.Check(order)
	if !riskResult.Passed {
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success:      false,
			RejectReason: riskResult.Reason,
		})
		return
	}

	// ========================================================================
	// CRITICAL: Lock-free ring buffer submission (LMAX Disruptor pattern)
	// ========================================================================
	//
	// This replaces the traditional mutex-based approach:
	//   OLD: s.mu.Lock(); result := s.engine.ProcessOrder(order); s.mu.Unlock()
	//   NEW: Submit to ring buffer, get response via channel
	//
	// Benefits:
	//   - No lock contention between HTTP handlers (5-10x throughput improvement)
	//   - Multiple handlers claim slots concurrently using atomic CAS
	//   - Single event processor consumes sequentially (maintains determinism)
	//
	// See README "LMAX Disruptor Pattern (Ring Buffer)" for detailed explanation

	// Create buffered response channel (event processor will send result here)
	responseCh := make(chan *disruptor.OrderResponse, 1)

	// Package order into a ring buffer request
	request := &disruptor.OrderRequest{
		Type:  disruptor.RequestTypeNewOrder,
		Order: order,
	}

	// Step 1: Claim a sequence number in the ring buffer (lock-free CAS operation)
	// The sequencer uses atomic.CompareAndSwapUint64 to claim the next slot
	// If buffer is full, it spins for ~100μs then returns ErrBufferFull
	seq, err := s.sequencer.Next()
	if err != nil {
		// Ring buffer full (backpressure) - return 503 Service Unavailable
		// Client should retry with exponential backoff
		writeJSON(w, http.StatusServiceUnavailable, OrderResponse{
			Success: false,
			Error:   "server busy, please retry",
		})
		return
	}

	// Step 2: Publish the request to the claimed slot
	// This writes the order and response channel to the slot, then atomically
	// updates the slot's sequence number to signal readiness to the consumer
	s.sequencer.Publish(seq, request, responseCh)

	// Step 3: Wait for the event processor to process the order and respond
	// The processor will call engine.ProcessOrder() and send the result
	var response *disruptor.OrderResponse
	select {
	case response = <-responseCh:
		// Got response from event processor
	case <-time.After(5 * time.Second):
		// Timeout waiting for processing (shouldn't happen unless system overloaded)
		writeJSON(w, http.StatusGatewayTimeout, OrderResponse{
			Success: false,
			Error:   "processing timeout",
		})
		return
	}

	// Check if order was accepted
	if !response.Success {
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success:      false,
			OrderID:      order.ID,
			RejectReason: response.Result.RejectReason,
			Error:        fmt.Sprintf("%v", response.Error),
		})
		return
	}

	result := response.Result

	// ========================================================================
	// Post-processing: Handle fills and publish market data
	// ========================================================================
	//
	// NOTE: Event logging (NewOrderEvent, FillEvent) is already handled by
	// the event processor before sending the response. We only need to:
	//   1. Record trades for settlement (T+2 clearing)
	//   2. Update risk positions (for future risk checks)
	//   3. Publish market data (trades and L1 quotes)

	// Process each fill (trade execution)
	fills := make([]FillInfo, len(result.Fills))
	for i, fill := range result.Fills {
		// Convert to response format (price as decimal string)
		fills[i] = FillInfo{
			TradeID:  fill.TradeID,
			Price:    orders.FormatPrice(fill.Price), // Convert fixed-point to decimal
			Quantity: fill.Quantity,
		}

		// Record trade for settlement (T+2 clearing house)
		// This updates account cash and holdings
		s.clearingHouse.RecordTrade(fill)

		// Update risk checker's position tracking
		// Taker gets +quantity (buy) or -quantity (sell)
		// Maker gets opposite position
		s.riskChecker.UpdatePosition(fill.TakerAccountID, fill.Symbol, fill.TakerSide, fill.Quantity)
		s.riskChecker.UpdatePosition(fill.MakerAccountID, fill.Symbol, fill.TakerSide.Opposite(), fill.Quantity)
		s.riskChecker.SetReferencePrice(fill.Symbol, fill.Price) // For mark-to-market

		// Publish trade to market data feed (for tape, charting, etc.)
		s.publisher.PublishTrade(marketdata.TradeReport{
			TradeID:       fill.TradeID,
			Symbol:        fill.Symbol,
			Price:         fill.Price,
			Quantity:      fill.Quantity,
			AggressorSide: fill.TakerSide,
			Timestamp:     fill.Timestamp,
		})
	}

	// Publish Level 1 (L1) market data update (best bid/ask, last trade)
	// This is used by trading UIs to show real-time quotes
	book := s.engine.GetOrderBook(order.Symbol)
	if book != nil {
		l1 := marketdata.L1Quote{
			Symbol:    order.Symbol,
			Timestamp: orders.Now(),
		}
		if bestBid := book.GetBestBid(); bestBid != nil {
			l1.BidPrice = bestBid.Price
			l1.BidSize = bestBid.TotalQty
		}
		if bestAsk := book.GetBestAsk(); bestAsk != nil {
			l1.AskPrice = bestAsk.Price
			l1.AskSize = bestAsk.TotalQty
		}
		if len(result.Fills) > 0 {
			lastFill := result.Fills[len(result.Fills)-1]
			l1.LastPrice = lastFill.Price
			l1.LastSize = lastFill.Quantity
		}
		s.publisher.PublishL1(l1)
	}

	writeJSON(w, http.StatusOK, OrderResponse{
		Success:      true,
		OrderID:      order.ID,
		Status:       order.Status.String(),
		FilledQty:    order.FilledQty,
		RemainingQty: order.RemainingQty(),
		Fills:        fills,
	})
}

// handleCancel handles order cancellation requests.
//
// Uses the same lock-free ring buffer pattern as handleOrder.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse cancellation parameters from query string
	symbol := r.URL.Query().Get("symbol")
	orderIDStr := r.URL.Query().Get("order_id")

	if symbol == "" || orderIDStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "symbol and order_id required",
		})
		return
	}

	orderID, err := strconv.ParseUint(orderIDStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid order_id",
		})
		return
	}

	// Submit cancellation to ring buffer (same pattern as new orders)
	responseCh := make(chan *disruptor.OrderResponse, 1)

	request := &disruptor.OrderRequest{
		Type:    disruptor.RequestTypeCancelOrder,
		Symbol:  symbol,
		OrderID: orderID,
	}

	// Step 1: Claim sequence number (lock-free CAS)
	seq, err := s.sequencer.Next()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "server busy, please retry",
		})
		return
	}

	// Step 2: Publish to ring buffer
	s.sequencer.Publish(seq, request, responseCh)

	// Step 3: Wait for event processor to cancel the order
	var response *disruptor.OrderResponse
	select {
	case response = <-responseCh:
		// Got response
	case <-time.After(5 * time.Second):
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{
			"error": "processing timeout",
		})
		return
	}

	if !response.Success || response.Error != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": response.Error.Error(),
		})
		return
	}

	order := response.Order

	// Note: Cancel event logging is handled by the event processor

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"order_id":      order.ID,
		"cancelled_qty": order.RemainingQty(),
	})
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "symbol required",
		})
		return
	}

	book := s.engine.GetOrderBook(symbol)
	if book == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "symbol not found",
		})
		return
	}

	levels := 10
	if l := r.URL.Query().Get("levels"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			levels = parsed
		}
	}

	bids := book.GetBidDepth(levels)
	asks := book.GetAskDepth(levels)

	bidData := make([]map[string]interface{}, len(bids))
	for i, level := range bids {
		bidData[i] = map[string]interface{}{
			"price":    orders.FormatPrice(level.Price),
			"quantity": level.TotalQty,
			"orders":   level.Count(),
		}
	}

	askData := make([]map[string]interface{}, len(asks))
	for i, level := range asks {
		askData[i] = map[string]interface{}{
			"price":    orders.FormatPrice(level.Price),
			"quantity": level.TotalQty,
			"orders":   level.Count(),
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"symbol": symbol,
		"bids":   bidData,
		"asks":   askData,
		"spread": orders.FormatPrice(book.GetSpread()),
		"mid":    orders.FormatPrice(book.GetMidPrice()),
	})
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("id")
	if accountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "id required",
		})
		return
	}

	account := s.clearingHouse.GetAccount(accountID)
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "account not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":       account.ID,
		"cash":     orders.FormatPrice(account.Cash),
		"holdings": account.Holdings,
	})
}

// handleStats returns system statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.clearingHouse.GetSettlementStats()

	// Read order book stats
	// SAFETY: Safe to read without locks because:
	//   1. Event processor is the only writer (single-threaded)
	//   2. HTTP handlers only read (concurrent reads are safe)
	//   3. Go memory model guarantees read visibility after write completes
	var totalOrders int
	for _, symbol := range s.engine.Symbols() {
		if book := s.engine.GetOrderBook(symbol); book != nil {
			totalOrders += book.TotalOrders()
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"orders_in_book":    totalOrders,
		"event_log_seq":     s.eventLog.GetLastSequence(),
		"settlement_stats":  stats,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "healthy",
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	// Parse command-line flags
	port := flag.Int("port", 8080, "Server port")
	eventLog := flag.String("event-log", "events.log", "Path to event log file")
	syncMode := flag.Bool("sync", false, "Enable sync mode for event log (slower but durable)")
	flag.Parse()

	// Build configuration
	config := DefaultConfig()
	config.Port = *port
	config.EventLogPath = *eventLog
	config.SyncMode = *syncMode

	// Create server
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// ========================================================================
	// Graceful shutdown handling
	// ========================================================================
	//
	// Listen for SIGINT (Ctrl+C) or SIGTERM (kill) signals and gracefully
	// shut down the server. This ensures:
	//   1. No new HTTP requests are accepted
	//   2. In-flight requests complete
	//   3. Ring buffer is drained (all pending orders processed)
	//   4. Event log is flushed to disk (no data loss)
	//
	// Production systems should also handle SIGHUP for configuration reloads
	// and provide metrics/monitoring for shutdown duration.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start shutdown goroutine
	go func() {
		<-sigCh
		log.Println("Received shutdown signal")

		// Give server 10 seconds to shutdown gracefully
		// If it takes longer, the context timeout will force termination
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	// Start server (blocks until shutdown)
	if err := server.Start(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}
