// Package disruptor implements the LMAX Disruptor pattern for lock-free,
// high-throughput order processing.
//
// The Disruptor pattern achieves high performance through:
// 1. Lock-free multi-producer coordination using CAS operations
// 2. Pre-allocated ring buffer to eliminate GC pressure
// 3. Cache-aligned data structures to prevent false sharing
// 4. Single-threaded consumer for deterministic processing
//
// Reference: https://lmax-exchange.github.io/disruptor/
package disruptor

import (
	"errors"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// RequestType identifies the type of request in the ring buffer.
type RequestType uint8

const (
	RequestTypeNewOrder RequestType = iota
	RequestTypeCancelOrder
)

// OrderRequest encapsulates an order processing request.
type OrderRequest struct {
	Type RequestType

	// For new orders
	Order *orders.Order

	// For cancellations
	Symbol  string
	OrderID uint64
}

// OrderResponse contains the execution result.
type OrderResponse struct {
	Success bool
	Result  *orders.ExecutionResult
	Order   *orders.Order
	Error   error
}

// RingBufferSlot represents a single slot in the ring buffer.
// Cache-aligned to 64 bytes to prevent false sharing between CPU cores.
type RingBufferSlot struct {
	// SequenceNum is the sequence number for this slot.
	// The slot is ready when SequenceNum matches expected sequence.
	SequenceNum uint64

	// Request contains the order processing request
	Request *OrderRequest

	// ResponseCh is where the result will be sent
	ResponseCh chan *OrderResponse

	// Padding to ensure 64-byte alignment (cache line size)
	// 8 (seq) + 8 (request ptr) + 8 (chan ptr) = 24 bytes used
	// Need 40 bytes padding to reach 64 bytes
	_ [40]byte
}

// RingBuffer is a lock-free, multi-producer, single-consumer ring buffer.
//
// Design:
// - Fixed size (must be power of 2 for fast modulo via bitwise AND)
// - Pre-allocated slots to avoid GC pressure
// - Atomic cursors for multi-producer coordination
// - Gating sequence to prevent overwriting unconsumed data
type RingBuffer struct {
	// bufferSize is the size of the ring buffer (must be power of 2)
	bufferSize uint64

	// indexMask for fast modulo operation (bufferSize - 1)
	indexMask uint64

	// slots are the pre-allocated buffer slots
	slots []RingBufferSlot

	// cursor is the write cursor (multi-producer, atomic CAS)
	// Tracks the highest claimed sequence number
	cursor uint64

	// consumerCursor is the read cursor (single consumer)
	// Tracks the next sequence to be consumed
	consumerCursor uint64

	// gatingSequence tracks the highest consumed sequence
	// Prevents producers from overwriting unconsumed data
	gatingSequence uint64

	// Padding to prevent false sharing with other data structures
	_ [40]byte
}

// Config holds ring buffer configuration.
type Config struct {
	// BufferSize is the number of slots in the ring buffer.
	// Must be a power of 2 (e.g., 1024, 4096, 8192).
	BufferSize uint64
}

// DefaultConfig returns reasonable defaults for the ring buffer.
func DefaultConfig() Config {
	return Config{
		BufferSize: 8192, // 8K slots, power of 2
	}
}

// NewRingBuffer creates a new ring buffer.
func NewRingBuffer(config Config) *RingBuffer {
	// Validate buffer size is power of 2
	if config.BufferSize == 0 || (config.BufferSize&(config.BufferSize-1)) != 0 {
		panic("BufferSize must be a power of 2")
	}

	rb := &RingBuffer{
		bufferSize:     config.BufferSize,
		indexMask:      config.BufferSize - 1,
		slots:          make([]RingBufferSlot, config.BufferSize),
		cursor:         0,
		consumerCursor: 1, // Start at 1 (will consume from sequence 1)
		gatingSequence: 0, // Initially, nothing has been consumed
	}

	// Initialize all slots with sequence numbers (not yet published)
	for i := uint64(0); i < config.BufferSize; i++ {
		rb.slots[i].SequenceNum = 0
	}

	return rb
}

// GetBufferSize returns the buffer size.
func (rb *RingBuffer) GetBufferSize() uint64 {
	return rb.bufferSize
}

// ErrBufferFull is returned when the ring buffer is full.
var ErrBufferFull = errors.New("ring buffer is full")
