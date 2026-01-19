// Package events defines event types for the event sourcing system.
//
// Event Sourcing Pattern:
// Instead of storing current state, we store all state changes (events).
// Current state can be reconstructed by replaying events from the beginning.
//
// Benefits:
// 1. Audit Trail: Complete history of every action (regulatory requirement)
// 2. Replay: Rebuild state after crash by replaying events
// 3. Debugging: Reproduce any bug by replaying to that point
// 4. Time Travel: Query historical state at any point in time
//
// In financial systems, event sourcing is often mandatory for regulatory
// compliance (MiFID II, SEC Rule 613 CAT).
package events

import (
	"github.com/rishav/order-matching-engine/internal/orders"
)

// EventType identifies the type of event.
type EventType uint8

const (
	EventTypeNewOrder EventType = iota + 1
	EventTypeCancelOrder
	EventTypeOrderAccepted
	EventTypeOrderRejected
	EventTypeFill
	EventTypeOrderCancelled
)

func (t EventType) String() string {
	switch t {
	case EventTypeNewOrder:
		return "NEW_ORDER"
	case EventTypeCancelOrder:
		return "CANCEL_ORDER"
	case EventTypeOrderAccepted:
		return "ORDER_ACCEPTED"
	case EventTypeOrderRejected:
		return "ORDER_REJECTED"
	case EventTypeFill:
		return "FILL"
	case EventTypeOrderCancelled:
		return "ORDER_CANCELLED"
	default:
		return "UNKNOWN"
	}
}

// Event is the base event structure.
// All events share these common fields.
type Event struct {
	SequenceNum uint64    // Global sequence number
	Timestamp   int64     // Nanoseconds since epoch
	Type        EventType // Event type
}

// NewOrderEvent represents a new order submission.
type NewOrderEvent struct {
	Event
	OrderID       uint64
	Symbol        string
	Side          orders.Side
	OrderType     orders.OrderType
	Price         int64
	Quantity      int64
	AccountID     string
	ClientOrderID string
}

// CancelOrderEvent represents an order cancellation request.
type CancelOrderEvent struct {
	Event
	OrderID   uint64
	Symbol    string
	AccountID string
}

// OrderAcceptedEvent indicates an order was accepted.
type OrderAcceptedEvent struct {
	Event
	OrderID     uint64
	Symbol      string
	RestingQty  int64 // Quantity added to book (0 if fully filled)
}

// OrderRejectedEvent indicates an order was rejected.
type OrderRejectedEvent struct {
	Event
	OrderID      uint64
	Symbol       string
	RejectReason string
}

// FillEvent represents a trade execution.
type FillEvent struct {
	Event
	TradeID        uint64
	Symbol         string
	Price          int64
	Quantity       int64
	MakerOrderID   uint64
	TakerOrderID   uint64
	MakerAccountID string
	TakerAccountID string
	TakerSide      orders.Side
}

// OrderCancelledEvent indicates an order was cancelled.
type OrderCancelledEvent struct {
	Event
	OrderID       uint64
	Symbol        string
	CancelledQty  int64 // Remaining quantity that was cancelled
	Reason        string
}
