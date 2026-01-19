// Package orders defines the core order types and related data structures
// for the order matching engine.
//
// Key Design Decisions:
//
// 1. Fixed-Point Arithmetic: Prices are stored as int64 in cents (1/100 of a dollar)
//    to avoid floating-point errors. For example, $150.25 is stored as 15025.
//    This is critical in financial systems where accumulated rounding errors
//    are unacceptable.
//
// 2. Sequence Numbers: Every order receives a globally unique, monotonically
//    increasing sequence number. This enables:
//    - Deterministic replay (rebuild state by replaying events in order)
//    - Fair ordering (prove orders were processed in arrival order)
//    - Gap detection (missing sequence = system problem)
//
// 3. Time Representation: Timestamps use nanoseconds since Unix epoch (int64)
//    for high precision without the overhead of time.Time struct.
package orders

import (
	"fmt"
	"time"
)

// Side represents the side of an order (buy or sell).
type Side int

const (
	SideBuy Side = iota
	SideSell
)

func (s Side) String() string {
	switch s {
	case SideBuy:
		return "BUY"
	case SideSell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

// Opposite returns the opposite side.
func (s Side) Opposite() Side {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// OrderType represents the type of order and its execution semantics.
type OrderType int

const (
	// OrderTypeLimit rests in the book until filled or cancelled.
	// Only executes at the specified price or better.
	OrderTypeLimit OrderType = iota

	// OrderTypeMarket executes immediately at the best available price.
	// No price protection - will fill at whatever price is available.
	OrderTypeMarket

	// OrderTypeIOC (Immediate-or-Cancel) executes immediately for whatever
	// quantity is available, then cancels any remaining quantity.
	// Useful when you want immediate execution but accept partial fills.
	OrderTypeIOC

	// OrderTypeFOK (Fill-or-Kill) must be filled entirely or not at all.
	// If the full quantity cannot be matched immediately, the entire order
	// is cancelled. No partial fills allowed.
	OrderTypeFOK
)

func (t OrderType) String() string {
	switch t {
	case OrderTypeLimit:
		return "LIMIT"
	case OrderTypeMarket:
		return "MARKET"
	case OrderTypeIOC:
		return "IOC"
	case OrderTypeFOK:
		return "FOK"
	default:
		return "UNKNOWN"
	}
}

// OrderStatus represents the current state of an order.
type OrderStatus int

const (
	// OrderStatusNew - order has been accepted but not yet processed
	OrderStatusNew OrderStatus = iota

	// OrderStatusPartiallyFilled - order has been partially executed
	OrderStatusPartiallyFilled

	// OrderStatusFilled - order has been completely filled
	OrderStatusFilled

	// OrderStatusCancelled - order was cancelled (by user or system)
	OrderStatusCancelled

	// OrderStatusRejected - order was rejected (failed validation/risk check)
	OrderStatusRejected
)

func (s OrderStatus) String() string {
	switch s {
	case OrderStatusNew:
		return "NEW"
	case OrderStatusPartiallyFilled:
		return "PARTIALLY_FILLED"
	case OrderStatusFilled:
		return "FILLED"
	case OrderStatusCancelled:
		return "CANCELLED"
	case OrderStatusRejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// Order represents a single order in the matching engine.
//
// Memory Layout Considerations:
// - Fields are ordered to minimize padding (largest first)
// - Total size: 88 bytes (fits in 1.5 cache lines)
// - No pointers except Symbol string (reduces GC pressure)
type Order struct {
	// ID is the unique identifier for this order, assigned by the exchange.
	ID uint64

	// SequenceNum is the global sequence number assigned when the order
	// enters the matching engine. Used for deterministic replay.
	SequenceNum uint64

	// Price in cents (fixed-point). For a $150.25 stock, this would be 15025.
	// For market orders, this field is ignored.
	Price int64

	// Quantity is the total number of shares in this order.
	Quantity int64

	// FilledQty is the number of shares that have been executed.
	// RemainingQty = Quantity - FilledQty
	FilledQty int64

	// Timestamp is the time the order was received, in nanoseconds since epoch.
	Timestamp int64

	// Symbol is the stock ticker symbol (e.g., "AAPL", "GOOGL").
	Symbol string

	// AccountID identifies the account that placed this order.
	AccountID string

	// ClientOrderID is an optional client-provided identifier for the order.
	ClientOrderID string

	// Side indicates whether this is a buy or sell order.
	Side Side

	// Type indicates the order type (Limit, Market, IOC, FOK).
	Type OrderType

	// Status is the current state of the order.
	Status OrderStatus
}

// RemainingQty returns the unfilled quantity of the order.
func (o *Order) RemainingQty() int64 {
	return o.Quantity - o.FilledQty
}

// IsFilled returns true if the order has been completely filled.
func (o *Order) IsFilled() bool {
	return o.FilledQty >= o.Quantity
}

// IsActive returns true if the order can still be matched.
func (o *Order) IsActive() bool {
	return o.Status == OrderStatusNew || o.Status == OrderStatusPartiallyFilled
}

// PriceStr returns the price formatted as a dollar string.
func (o *Order) PriceStr() string {
	return FormatPrice(o.Price)
}

// String returns a human-readable representation of the order.
func (o *Order) String() string {
	return fmt.Sprintf("Order{ID:%d, %s %s %d@%s, Filled:%d, Status:%s}",
		o.ID, o.Side, o.Symbol, o.Quantity, o.PriceStr(), o.FilledQty, o.Status)
}

// Fill represents a single execution (trade) between two orders.
//
// When a new order matches against resting orders, one Fill is created
// for each resting order that participates in the execution.
type Fill struct {
	// TradeID is the unique identifier for this execution.
	TradeID uint64

	// MakerOrderID is the ID of the resting (passive) order.
	MakerOrderID uint64

	// TakerOrderID is the ID of the incoming (aggressive) order.
	TakerOrderID uint64

	// Price is the execution price in cents.
	// Always the maker's price (price improvement for taker).
	Price int64

	// Quantity is the number of shares executed.
	Quantity int64

	// Timestamp is when the fill occurred, in nanoseconds since epoch.
	Timestamp int64

	// Symbol is the stock ticker.
	Symbol string

	// MakerAccountID is the account of the resting order.
	MakerAccountID string

	// TakerAccountID is the account of the incoming order.
	TakerAccountID string

	// TakerSide indicates whether the taker was buying or selling.
	TakerSide Side
}

// String returns a human-readable representation of the fill.
func (f *Fill) String() string {
	return fmt.Sprintf("Fill{Trade:%d, %d shares@%s, Maker:%d, Taker:%d}",
		f.TradeID, f.Quantity, FormatPrice(f.Price), f.MakerOrderID, f.TakerOrderID)
}

// Trade represents a completed trade from the perspective of reporting.
// It combines information from both sides of the execution.
type Trade struct {
	ID            uint64
	Symbol        string
	Price         int64
	Quantity      int64
	BuyOrderID    uint64
	SellOrderID   uint64
	BuyerAccount  string
	SellerAccount string
	Timestamp     int64
	SequenceNum   uint64
}

// ExecutionResult contains the outcome of processing an order.
type ExecutionResult struct {
	// Order is the processed order with updated status and filled quantity.
	Order *Order

	// Fills contains all executions that occurred.
	Fills []Fill

	// Accepted indicates if the order was accepted into the system.
	Accepted bool

	// RejectReason explains why the order was rejected (if applicable).
	RejectReason string

	// RestingQty is the quantity that was added to the order book
	// (for limit orders that didn't fully match).
	RestingQty int64
}

// FormatPrice converts a price in cents to a dollar string.
func FormatPrice(cents int64) string {
	dollars := cents / 100
	remaining := cents % 100
	if remaining < 0 {
		remaining = -remaining
	}
	return fmt.Sprintf("$%d.%02d", dollars, remaining)
}

// ParsePrice converts a dollar amount to cents.
// For example, 150.25 becomes 15025.
func ParsePrice(dollars float64) int64 {
	return int64(dollars * 100)
}

// Now returns the current time in nanoseconds since epoch.
func Now() int64 {
	return time.Now().UnixNano()
}
