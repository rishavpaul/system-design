package orderbook

import (
	"fmt"
	"strings"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// OrderBook maintains the buy (bid) and sell (ask) sides of the market.
//
// Architecture:
//
//	                    OrderBook
//	                        │
//	       ┌────────────────┴────────────────┐
//	       │                                 │
//	    Bids (RBTree)                   Asks (RBTree)
//	    descending=true                 descending=false
//	       │                                 │
//	    PriceLevel                       PriceLevel
//	    (sorted high→low)                (sorted low→high)
//	       │                                 │
//	    OrderQueue                       OrderQueue
//	    (FIFO linked list)               (FIFO linked list)
//
// Key Design Decisions:
//
// 1. Two Red-Black Trees: One for bids (highest first), one for asks (lowest first)
//    - O(1) access to best bid/ask via cached min/max pointers
//    - O(log P) insert/delete where P = number of price levels
//
// 2. Order ID Map: Hash map from order ID to OrderNode
//    - O(1) cancel by order ID (no search required)
//    - Critical for high-frequency trading where cancels are common
//
// 3. Price-Time Priority: Implemented via:
//    - Red-black tree for price priority (best price first)
//    - FIFO queue at each price level for time priority (first order first)
type OrderBook struct {
	symbol string
	bids   *RBTree             // Buy orders, sorted by price descending
	asks   *RBTree             // Sell orders, sorted by price ascending
	orders map[uint64]*OrderNode // Order ID -> Node for O(1) cancel
}

// NewOrderBook creates a new order book for the given symbol.
func NewOrderBook(symbol string) *OrderBook {
	return &OrderBook{
		symbol: symbol,
		bids:   NewRBTree(true),  // descending: true (highest price first)
		asks:   NewRBTree(false), // descending: false (lowest price first)
		orders: make(map[uint64]*OrderNode),
	}
}

// Symbol returns the symbol this order book is for.
func (ob *OrderBook) Symbol() string {
	return ob.symbol
}

// AddOrder adds an order to the appropriate side of the book.
// Returns an error if the order already exists.
// Time complexity: O(log P) where P = number of price levels
func (ob *OrderBook) AddOrder(order *orders.Order) error {
	if _, exists := ob.orders[order.ID]; exists {
		return fmt.Errorf("order %d already exists", order.ID)
	}

	// Get the appropriate tree
	tree := ob.getTree(order.Side)

	// Find or create price level
	level := tree.Get(order.Price)
	if level == nil {
		level = NewPriceLevel(order.Price)
		tree.Insert(level)
	}

	// Add order to the price level's queue
	node := level.Append(order)

	// Track order for O(1) cancellation
	ob.orders[order.ID] = node

	return nil
}

// CancelOrder removes an order from the book.
// Returns the cancelled order, or nil if not found.
// Time complexity: O(1) for the removal, O(log P) if price level becomes empty
func (ob *OrderBook) CancelOrder(orderID uint64) *orders.Order {
	node, exists := ob.orders[orderID]
	if !exists {
		return nil
	}

	order := node.Order
	level := node.level
	tree := ob.getTree(order.Side)

	// Remove order from the queue
	level.Remove(node)

	// Remove from tracking map
	delete(ob.orders, orderID)

	// If price level is empty, remove it from the tree
	if level.IsEmpty() {
		tree.Delete(level.Price)
	}

	return order
}

// GetOrder retrieves an order by ID.
// Time complexity: O(1)
func (ob *OrderBook) GetOrder(orderID uint64) *orders.Order {
	node, exists := ob.orders[orderID]
	if !exists {
		return nil
	}
	return node.Order
}

// GetBestBid returns the highest bid price level, or nil if no bids.
// Time complexity: O(1)
func (ob *OrderBook) GetBestBid() *PriceLevel {
	return ob.bids.Min()
}

// GetBestAsk returns the lowest ask price level, or nil if no asks.
// Time complexity: O(1)
func (ob *OrderBook) GetBestAsk() *PriceLevel {
	return ob.asks.Min()
}

// GetSpread returns the difference between best ask and best bid.
// Returns 0 if either side is empty.
func (ob *OrderBook) GetSpread() int64 {
	bestBid := ob.GetBestBid()
	bestAsk := ob.GetBestAsk()
	if bestBid == nil || bestAsk == nil {
		return 0
	}
	return bestAsk.Price - bestBid.Price
}

// GetMidPrice returns the midpoint between best bid and ask.
// Returns 0 if either side is empty.
func (ob *OrderBook) GetMidPrice() int64 {
	bestBid := ob.GetBestBid()
	bestAsk := ob.GetBestAsk()
	if bestBid == nil || bestAsk == nil {
		return 0
	}
	return (bestBid.Price + bestAsk.Price) / 2
}

// BidLevels returns the number of distinct bid price levels.
func (ob *OrderBook) BidLevels() int {
	return ob.bids.Size()
}

// AskLevels returns the number of distinct ask price levels.
func (ob *OrderBook) AskLevels() int {
	return ob.asks.Size()
}

// TotalOrders returns the total number of orders in the book.
func (ob *OrderBook) TotalOrders() int {
	return len(ob.orders)
}

// GetBidDepth returns the top N bid price levels.
// If levels <= 0, returns all levels.
func (ob *OrderBook) GetBidDepth(levels int) []*PriceLevel {
	return ob.getDepth(ob.bids, levels)
}

// GetAskDepth returns the top N ask price levels.
// If levels <= 0, returns all levels.
func (ob *OrderBook) GetAskDepth(levels int) []*PriceLevel {
	return ob.getDepth(ob.asks, levels)
}

// getDepth returns the top N levels from a tree.
func (ob *OrderBook) getDepth(tree *RBTree, maxLevels int) []*PriceLevel {
	result := make([]*PriceLevel, 0)
	count := 0

	tree.ForEach(func(level *PriceLevel) bool {
		result = append(result, level)
		count++
		if maxLevels > 0 && count >= maxLevels {
			return false // Stop iteration
		}
		return true
	})

	return result
}

// UpdateOrderQuantity updates the remaining quantity of an order.
// Used when an order is partially filled.
// Time complexity: O(1)
func (ob *OrderBook) UpdateOrderQuantity(orderID uint64, fillQty int64) error {
	node, exists := ob.orders[orderID]
	if !exists {
		return fmt.Errorf("order %d not found", orderID)
	}

	order := node.Order
	order.FilledQty += fillQty

	// Update the price level's total quantity
	node.level.UpdateQuantity(-fillQty)

	// If fully filled, remove from book
	if order.IsFilled() {
		ob.CancelOrder(orderID)
	}

	return nil
}

// RemoveFilledOrders removes all fully filled orders from a price level.
// Returns the number of orders removed.
func (ob *OrderBook) RemoveFilledOrders(level *PriceLevel, side orders.Side) int {
	removed := 0
	node := level.Head()

	for node != nil {
		next := node.next
		if node.Order.IsFilled() {
			level.Remove(node)
			delete(ob.orders, node.Order.ID)
			removed++
		}
		node = next
	}

	// Remove empty price level
	if level.IsEmpty() {
		tree := ob.getTree(side)
		tree.Delete(level.Price)
	}

	return removed
}

// getTree returns the appropriate tree for the given side.
func (ob *OrderBook) getTree(side orders.Side) *RBTree {
	if side == orders.SideBuy {
		return ob.bids
	}
	return ob.asks
}

// String returns a human-readable representation of the order book.
func (ob *OrderBook) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== %s Order Book ===\n", ob.symbol))

	// Asks (show in reverse so lowest is at bottom, closest to bids)
	asks := ob.GetAskDepth(5)
	sb.WriteString("ASKS:\n")
	for i := len(asks) - 1; i >= 0; i-- {
		level := asks[i]
		sb.WriteString(fmt.Sprintf("  %s: %d shares (%d orders)\n",
			orders.FormatPrice(level.Price), level.TotalQty, level.Count()))
	}

	// Spread
	spread := ob.GetSpread()
	if spread > 0 {
		sb.WriteString(fmt.Sprintf("--- Spread: %s ---\n", orders.FormatPrice(spread)))
	} else {
		sb.WriteString("--- No Spread ---\n")
	}

	// Bids
	bids := ob.GetBidDepth(5)
	sb.WriteString("BIDS:\n")
	for _, level := range bids {
		sb.WriteString(fmt.Sprintf("  %s: %d shares (%d orders)\n",
			orders.FormatPrice(level.Price), level.TotalQty, level.Count()))
	}

	return sb.String()
}
