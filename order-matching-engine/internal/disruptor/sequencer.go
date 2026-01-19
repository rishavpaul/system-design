package disruptor

import (
	"runtime"
	"sync/atomic"
)

// Sequencer coordinates access to the ring buffer using atomic CAS operations.
//
// Design:
// - Next() claims a sequence number for a producer
// - Publish() writes the request to the claimed slot
// - Multi-producer safe through CAS loop
// - Backpressure via spinning and eventual rejection
type Sequencer struct {
	rb *RingBuffer
}

// NewSequencer creates a new sequencer for the given ring buffer.
func NewSequencer(rb *RingBuffer) *Sequencer {
	return &Sequencer{
		rb: rb,
	}
}

// Next claims the next sequence number for writing.
//
// This method is lock-free and multi-producer safe using atomic CAS.
// If the buffer is full, it will spin briefly (~100μs) and then return ErrBufferFull.
//
// Returns:
// - sequence number on success
// - ErrBufferFull if buffer is full after spinning
func (s *Sequencer) Next() (uint64, error) {
	const maxSpins = 10000 // ~100μs on modern CPU (10ns per iteration)

	for spins := 0; spins < maxSpins; spins++ {
		// Load current cursor
		current := atomic.LoadUint64(&s.rb.cursor)
		next := current + 1

		// Check if we would overwrite unconsumed data
		// We can only fill up to (gatingSequence + bufferSize) slots
		cachedGatingSequence := atomic.LoadUint64(&s.rb.gatingSequence)
		availableSequence := cachedGatingSequence + s.rb.bufferSize

		// If next would exceed available space, buffer is full
		if next > availableSequence {
			// Buffer is full, yield to consumer
			runtime.Gosched()
			continue
		}

		// Try to claim this sequence number using CAS
		if atomic.CompareAndSwapUint64(&s.rb.cursor, current, next) {
			return next, nil
		}

		// CAS failed, another producer won the race, retry
	}

	// Exhausted spins, buffer is full
	return 0, ErrBufferFull
}

// Publish writes a request to the claimed sequence slot.
//
// This method must only be called after successfully claiming a sequence via Next().
// It writes the request and response channel to the slot, then updates the slot's
// sequence number to signal readiness to the consumer.
//
// Memory ordering:
// - All writes to the slot must complete before the sequence number update
// - The atomic store provides a release barrier ensuring visibility
func (s *Sequencer) Publish(seq uint64, request *OrderRequest, responseCh chan *OrderResponse) {
	// Calculate slot index using fast modulo (bitwise AND with mask)
	index := seq & s.rb.indexMask
	slot := &s.rb.slots[index]

	// Write request data to slot
	slot.Request = request
	slot.ResponseCh = responseCh

	// Memory barrier: ensure all writes above are visible before sequence update
	// The atomic store with sequential consistency guarantees this
	atomic.StoreUint64(&slot.SequenceNum, seq)
}
