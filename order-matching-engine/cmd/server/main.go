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
type Server struct {
	engine        *matching.Engine
	riskChecker   *risk.Checker
	eventLog      *events.EventLog
	publisher     *marketdata.Publisher
	clearingHouse *settlement.ClearingHouse

	// Disruptor components for lock-free processing
	ringBuffer     *disruptor.RingBuffer
	sequencer      *disruptor.Sequencer
	eventProcessor *disruptor.EventProcessor

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
	// Create event log
	eventLog, err := events.NewEventLog(events.EventLogConfig{
		Path:     config.EventLogPath,
		SyncMode: config.SyncMode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create event log: %w", err)
	}

	// Create components
	engine := matching.NewEngine()
	for _, symbol := range config.Symbols {
		engine.AddSymbol(symbol)
	}

	riskChecker := risk.NewChecker(risk.DefaultConfig())
	publisher := marketdata.NewPublisher(1000)
	clearingHouse := settlement.NewClearingHouse()

	// Create some test accounts
	for _, acct := range []string{"TRADER1", "TRADER2", "MM1", "MM2"} {
		clearingHouse.GetOrCreateAccount(acct, 10000000) // $100,000 each
	}

	// Create disruptor components for lock-free processing
	ringBuffer := disruptor.NewRingBuffer(disruptor.DefaultConfig())
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

	// Start event processor
	s.eventProcessor.Start()

	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("Shutting down server...")

	// Stop accepting new HTTP requests
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return err
	}

	// Shutdown event processor (drains ring buffer and flushes events)
	s.eventProcessor.Shutdown()

	// Close event log
	if err := s.eventLog.Close(); err != nil {
		return err
	}

	// Close publisher
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

	// Parse price
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
		price = orders.ParsePrice(priceFloat)
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

	// Run risk checks
	riskResult := s.riskChecker.Check(order)
	if !riskResult.Passed {
		writeJSON(w, http.StatusBadRequest, OrderResponse{
			Success:      false,
			RejectReason: riskResult.Reason,
		})
		return
	}

	// Submit to ring buffer for lock-free processing
	responseCh := make(chan *disruptor.OrderResponse, 1)

	request := &disruptor.OrderRequest{
		Type:  disruptor.RequestTypeNewOrder,
		Order: order,
	}

	// Claim sequence in ring buffer
	seq, err := s.sequencer.Next()
	if err != nil {
		// Ring buffer full, return 503 Service Unavailable
		writeJSON(w, http.StatusServiceUnavailable, OrderResponse{
			Success: false,
			Error:   "server busy, please retry",
		})
		return
	}

	// Publish request to ring buffer
	s.sequencer.Publish(seq, request, responseCh)

	// Wait for response with timeout
	var response *disruptor.OrderResponse
	select {
	case response = <-responseCh:
		// Got response
	case <-time.After(5 * time.Second):
		// Timeout waiting for processing
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

	// Note: Event logging is handled by the event processor

	// Process fills
	fills := make([]FillInfo, len(result.Fills))
	for i, fill := range result.Fills {
		fills[i] = FillInfo{
			TradeID:  fill.TradeID,
			Price:    orders.FormatPrice(fill.Price),
			Quantity: fill.Quantity,
		}

		// Note: Fill event logging is handled by the event processor

		// Record trade for settlement
		s.clearingHouse.RecordTrade(fill)

		// Update risk positions
		s.riskChecker.UpdatePosition(fill.TakerAccountID, fill.Symbol, fill.TakerSide, fill.Quantity)
		s.riskChecker.UpdatePosition(fill.MakerAccountID, fill.Symbol, fill.TakerSide.Opposite(), fill.Quantity)
		s.riskChecker.SetReferencePrice(fill.Symbol, fill.Price)

		// Publish market data
		s.publisher.PublishTrade(marketdata.TradeReport{
			TradeID:       fill.TradeID,
			Symbol:        fill.Symbol,
			Price:         fill.Price,
			Quantity:      fill.Quantity,
			AggressorSide: fill.TakerSide,
			Timestamp:     fill.Timestamp,
		})
	}

	// Publish L1 update
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

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

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

	// Submit to ring buffer
	responseCh := make(chan *disruptor.OrderResponse, 1)

	request := &disruptor.OrderRequest{
		Type:    disruptor.RequestTypeCancelOrder,
		Symbol:  symbol,
		OrderID: orderID,
	}

	// Claim sequence
	seq, err := s.sequencer.Next()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "server busy, please retry",
		})
		return
	}

	// Publish request
	s.sequencer.Publish(seq, request, responseCh)

	// Wait for response
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

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.clearingHouse.GetSettlementStats()

	// Note: Safe to read without lock since event processor is the only writer
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
	port := flag.Int("port", 8080, "Server port")
	eventLog := flag.String("event-log", "events.log", "Path to event log file")
	syncMode := flag.Bool("sync", false, "Enable sync mode for event log (slower but durable)")
	flag.Parse()

	config := DefaultConfig()
	config.Port = *port
	config.EventLogPath = *eventLog
	config.SyncMode = *syncMode

	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Handle shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Received shutdown signal")
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	if err := server.Start(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}
