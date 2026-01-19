// Package orderbook implements the limit order book data structure.
//
// The order book maintains buy (bid) and sell (ask) orders organized by price.
// At each price level, orders are stored in a FIFO queue to implement
// price-time priority matching.
package orderbook

import (
	"github.com/rishav/order-matching-engine/internal/orders"
)

// OrderNode is a node in the doubly-linked list of orders at a price level.
// Using a doubly-linked list enables O(1) removal from anywhere in the queue,
// which is critical for fast order cancellation.
type OrderNode struct {
	Order *orders.Order
	prev  *OrderNode
	next  *OrderNode
	level *PriceLevel // Back-pointer for O(1) removal
}

// Next returns the next node in the queue.
func (n *OrderNode) Next() *OrderNode {
	return n.next
}

// PriceLevel represents all orders at a single price point.
//
// Design Rationale:
// - Orders at the same price are stored in arrival order (FIFO)
// - Doubly-linked list allows O(1) insertion at tail and O(1) removal anywhere
// - TotalQty is maintained for quick depth queries without iterating
//
// Example:
//
//	Price Level $150.25:
//	  Head -> [Order1: 100 shares] <-> [Order2: 50 shares] <-> [Order3: 75 shares] <- Tail
//	  TotalQty: 225 shares
type PriceLevel struct {
	Price    int64      // Price in cents (e.g., 15025 = $150.25)
	head     *OrderNode // First order (oldest, highest priority)
	tail     *OrderNode // Last order (newest, lowest priority)
	count    int        // Number of orders at this level
	TotalQty int64      // Sum of all order quantities (for quick depth queries)
}

// NewPriceLevel creates a new empty price level.
func NewPriceLevel(price int64) *PriceLevel {
	return &PriceLevel{
		Price: price,
	}
}

// Count returns the number of orders at this price level.
func (pl *PriceLevel) Count() int {
	return pl.count
}

// IsEmpty returns true if there are no orders at this level.
func (pl *PriceLevel) IsEmpty() bool {
	return pl.count == 0
}

// Head returns the first order node (highest priority).
func (pl *PriceLevel) Head() *OrderNode {
	return pl.head
}

// Append adds an order to the end of the queue (lowest priority at this price).
// Returns the OrderNode for O(1) cancellation later.
// Time complexity: O(1)
func (pl *PriceLevel) Append(order *orders.Order) *OrderNode {
	node := &OrderNode{
		Order: order,
		level: pl,
	}

	if pl.tail == nil {
		// Empty list
		pl.head = node
		pl.tail = node
	} else {
		// Add to tail
		node.prev = pl.tail
		pl.tail.next = node
		pl.tail = node
	}

	pl.count++
	pl.TotalQty += order.RemainingQty()
	return node
}

// Remove removes a node from the queue.
// Time complexity: O(1) due to doubly-linked list.
func (pl *PriceLevel) Remove(node *OrderNode) {
	if node == nil {
		return
	}

	// Update quantity before removal
	pl.TotalQty -= node.Order.RemainingQty()
	pl.count--

	// Update links
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		// Removing head
		pl.head = node.next
	}

	if node.next != nil {
		node.next.prev = node.prev
	} else {
		// Removing tail
		pl.tail = node.prev
	}

	// Clear node references to help GC
	node.prev = nil
	node.next = nil
	node.level = nil
}

// PopFront removes and returns the first order (highest priority).
// Returns nil if the level is empty.
// Time complexity: O(1)
func (pl *PriceLevel) PopFront() *orders.Order {
	if pl.head == nil {
		return nil
	}

	node := pl.head
	order := node.Order

	pl.TotalQty -= order.RemainingQty()
	pl.count--

	pl.head = node.next
	if pl.head != nil {
		pl.head.prev = nil
	} else {
		pl.tail = nil
	}

	// Clear node references
	node.next = nil
	node.level = nil

	return order
}

// UpdateQuantity adjusts TotalQty when an order is partially filled.
// Called when an order in this level gets a fill.
func (pl *PriceLevel) UpdateQuantity(delta int64) {
	pl.TotalQty += delta
}

// Orders returns a slice of all orders at this level (for debugging/display).
// Note: This allocates memory, use sparingly.
func (pl *PriceLevel) Orders() []*orders.Order {
	result := make([]*orders.Order, 0, pl.count)
	for node := pl.head; node != nil; node = node.next {
		result = append(result, node.Order)
	}
	return result
}
