// Package matching implements the order matching engine.
//
// The matching engine is the heart of the exchange. It processes incoming orders
// and matches them against resting orders in the order book using price-time
// priority (FIFO at each price level).
//
// Architecture: Single-Threaded Core (LMAX Disruptor Pattern)
//
// Why single-threaded?
// 1. Determinism: Same input sequence always produces same output
// 2. No locks: Eliminates contention in the hot path
// 3. Replay: Can rebuild state by replaying event log
// 4. Simplicity: No race conditions to debug
//
// Real exchanges like LMAX achieve 6 million orders/second with this pattern.
// The key insight is that matching logic is CPU-bound, not I/O-bound, so
// parallelism doesn't help - it only adds overhead.
package matching

import (
	"fmt"
	"sync/atomic"

	"github.com/rishav/order-matching-engine/internal/orderbook"
	"github.com/rishav/order-matching-engine/internal/orders"
)

// Engine is the single-threaded order matching engine.
//
// Thread Safety: The Process method must only be called from a single goroutine.
// External synchronization is handled by the sequencer/ring buffer that feeds
// events to the engine.
type Engine struct {
	orderBooks  map[string]*orderbook.OrderBook
	sequenceNum uint64 // Global sequence number
	tradeID     uint64 // Global trade ID counter
	orderID     uint64 // Global order ID counter
}

// NewEngine creates a new matching engine.
func NewEngine() *Engine {
	return &Engine{
		orderBooks: make(map[string]*orderbook.OrderBook),
	}
}

// AddSymbol adds a new tradable symbol to the engine.
func (e *Engine) AddSymbol(symbol string) {
	if _, exists := e.orderBooks[symbol]; !exists {
		e.orderBooks[symbol] = orderbook.NewOrderBook(symbol)
	}
}

// GetOrderBook returns the order book for a symbol.
func (e *Engine) GetOrderBook(symbol string) *orderbook.OrderBook {
	return e.orderBooks[symbol]
}

// NextOrderID generates the next order ID.
func (e *Engine) NextOrderID() uint64 {
	return atomic.AddUint64(&e.orderID, 1)
}

// nextTradeID generates the next trade ID.
func (e *Engine) nextTradeID() uint64 {
	return atomic.AddUint64(&e.tradeID, 1)
}

// nextSequence generates the next sequence number.
func (e *Engine) nextSequence() uint64 {
	return atomic.AddUint64(&e.sequenceNum, 1)
}

// ProcessOrder processes an incoming order and returns the execution result.
//
// This is the main entry point for order processing. It:
// 1. Validates the order
// 2. Assigns sequence number and order ID
// 3. Attempts to match against resting orders
// 4. Places any remaining quantity in the book (for limit orders)
//
// Time complexity: O(M * log P) where M = number of fills, P = price levels
func (e *Engine) ProcessOrder(order *orders.Order) *orders.ExecutionResult {
	result := &orders.ExecutionResult{
		Order:    order,
		Fills:    make([]orders.Fill, 0),
		Accepted: false,
	}

	// Validate
	book := e.orderBooks[order.Symbol]
	if book == nil {
		result.RejectReason = fmt.Sprintf("unknown symbol: %s", order.Symbol)
		order.Status = orders.OrderStatusRejected
		return result
	}

	if order.Quantity <= 0 {
		result.RejectReason = "quantity must be positive"
		order.Status = orders.OrderStatusRejected
		return result
	}

	if order.Type == orders.OrderTypeLimit && order.Price <= 0 {
		result.RejectReason = "limit order must have positive price"
		order.Status = orders.OrderStatusRejected
		return result
	}

	// Assign IDs
	if order.ID == 0 {
		order.ID = e.NextOrderID()
	}
	order.SequenceNum = e.nextSequence()
	if order.Timestamp == 0 {
		order.Timestamp = orders.Now()
	}
	order.Status = orders.OrderStatusNew
	result.Accepted = true

	// Match the order
	fills := e.matchOrder(order, book)
	result.Fills = fills

	// Update order status based on fills
	if order.IsFilled() {
		order.Status = orders.OrderStatusFilled
	} else if order.FilledQty > 0 {
		order.Status = orders.OrderStatusPartiallyFilled
	}

	// Handle remaining quantity based on order type
	remainingQty := order.RemainingQty()
	if remainingQty > 0 {
		switch order.Type {
		case orders.OrderTypeMarket:
			// Market orders that can't fully fill are cancelled
			order.Status = orders.OrderStatusCancelled
			result.RejectReason = "insufficient liquidity"

		case orders.OrderTypeIOC:
			// IOC: Immediate-or-Cancel - cancel unfilled portion
			order.Status = orders.OrderStatusCancelled

		case orders.OrderTypeFOK:
			// FOK: Fill-or-Kill - should have been handled in matchOrder
			// If we get here with remaining qty, it means no fill was possible
			order.Status = orders.OrderStatusCancelled
			result.RejectReason = "could not fill entire quantity"

		case orders.OrderTypeLimit:
			// Limit orders rest in the book
			book.AddOrder(order)
			result.RestingQty = remainingQty
		}
	}

	return result
}

// matchOrder attempts to match an incoming order against resting orders.
func (e *Engine) matchOrder(order *orders.Order, book *orderbook.OrderBook) []orders.Fill {
	var fills []orders.Fill

	// FOK orders need special handling - check if we can fill entirely first
	if order.Type == orders.OrderTypeFOK {
		if !e.canFillEntirely(order, book) {
			return fills // Empty - order will be cancelled
		}
	}

	// Determine which side of the book to match against
	var getMatchLevel func() *orderbook.PriceLevel
	var priceAcceptable func(bookPrice int64) bool

	if order.Side == orders.SideBuy {
		// Buy order matches against asks (sell orders)
		getMatchLevel = book.GetBestAsk
		priceAcceptable = func(bookPrice int64) bool {
			// For market orders, any price is acceptable
			if order.Type == orders.OrderTypeMarket {
				return true
			}
			// For limit orders, book price must be <= order price
			return bookPrice <= order.Price
		}
	} else {
		// Sell order matches against bids (buy orders)
		getMatchLevel = book.GetBestBid
		priceAcceptable = func(bookPrice int64) bool {
			if order.Type == orders.OrderTypeMarket {
				return true
			}
			// For limit orders, book price must be >= order price
			return bookPrice >= order.Price
		}
	}

	// Match against resting orders
	for order.RemainingQty() > 0 {
		level := getMatchLevel()
		if level == nil {
			break // No more resting orders
		}

		if !priceAcceptable(level.Price) {
			break // Price doesn't match
		}

		// Match against orders at this price level (FIFO)
		for node := level.Head(); node != nil && order.RemainingQty() > 0; {
			makerOrder := node.Order
			nextNode := node // Save for iteration

			// Calculate fill quantity
			fillQty := min(order.RemainingQty(), makerOrder.RemainingQty())

			// Create fill record
			fill := orders.Fill{
				TradeID:        e.nextTradeID(),
				MakerOrderID:   makerOrder.ID,
				TakerOrderID:   order.ID,
				Price:          level.Price, // Execute at maker's price (price improvement for taker)
				Quantity:       fillQty,
				Timestamp:      orders.Now(),
				Symbol:         order.Symbol,
				MakerAccountID: makerOrder.AccountID,
				TakerAccountID: order.AccountID,
				TakerSide:      order.Side,
			}
			fills = append(fills, fill)

			// Update quantities
			order.FilledQty += fillQty
			makerOrder.FilledQty += fillQty

			// Update maker order status
			if makerOrder.IsFilled() {
				makerOrder.Status = orders.OrderStatusFilled
			} else {
				makerOrder.Status = orders.OrderStatusPartiallyFilled
			}

			// Move to next node before potentially removing current
			nextNode = nextNode.Next()

			// Remove filled maker order from book
			if makerOrder.IsFilled() {
				book.CancelOrder(makerOrder.ID)
			} else {
				// Update the level's total quantity
				level.UpdateQuantity(-fillQty)
			}

			node = nextNode
		}

		// Check if level is now empty (shouldn't happen due to CancelOrder, but safety check)
		if level.IsEmpty() {
			break
		}
	}

	return fills
}

// canFillEntirely checks if a FOK order can be completely filled.
func (e *Engine) canFillEntirely(order *orders.Order, book *orderbook.OrderBook) bool {
	remainingQty := order.Quantity
	var levelIter func(func(*orderbook.PriceLevel) bool)
	var priceOK func(int64) bool

	if order.Side == orders.SideBuy {
		levelIter = func(fn func(*orderbook.PriceLevel) bool) {
			for level := book.GetBestAsk(); level != nil; {
				if !fn(level) {
					return
				}
				// Get next level (inefficient but FOK is rare)
				asks := book.GetAskDepth(0)
				found := false
				for i, l := range asks {
					if l.Price == level.Price && i+1 < len(asks) {
						level = asks[i+1]
						found = true
						break
					}
				}
				if !found {
					return
				}
			}
		}
		priceOK = func(p int64) bool {
			return order.Type == orders.OrderTypeMarket || p <= order.Price
		}
	} else {
		levelIter = func(fn func(*orderbook.PriceLevel) bool) {
			for level := book.GetBestBid(); level != nil; {
				if !fn(level) {
					return
				}
				bids := book.GetBidDepth(0)
				found := false
				for i, l := range bids {
					if l.Price == level.Price && i+1 < len(bids) {
						level = bids[i+1]
						found = true
						break
					}
				}
				if !found {
					return
				}
			}
		}
		priceOK = func(p int64) bool {
			return order.Type == orders.OrderTypeMarket || p >= order.Price
		}
	}

	// Check available quantity
	levelIter(func(level *orderbook.PriceLevel) bool {
		if !priceOK(level.Price) {
			return false
		}
		availableQty := level.TotalQty
		if availableQty >= remainingQty {
			remainingQty = 0
			return false
		}
		remainingQty -= availableQty
		return true
	})

	return remainingQty == 0
}

// CancelOrder cancels an existing order.
func (e *Engine) CancelOrder(symbol string, orderID uint64) (*orders.Order, error) {
	book := e.orderBooks[symbol]
	if book == nil {
		return nil, fmt.Errorf("unknown symbol: %s", symbol)
	}

	order := book.CancelOrder(orderID)
	if order == nil {
		return nil, fmt.Errorf("order %d not found", orderID)
	}

	order.Status = orders.OrderStatusCancelled
	return order, nil
}

// GetOrder retrieves an order by symbol and ID.
func (e *Engine) GetOrder(symbol string, orderID uint64) *orders.Order {
	book := e.orderBooks[symbol]
	if book == nil {
		return nil
	}
	return book.GetOrder(orderID)
}

// Symbols returns all tradable symbols.
func (e *Engine) Symbols() []string {
	symbols := make([]string, 0, len(e.orderBooks))
	for s := range e.orderBooks {
		symbols = append(symbols, s)
	}
	return symbols
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
