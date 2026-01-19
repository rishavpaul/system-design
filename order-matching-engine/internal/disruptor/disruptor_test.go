package disruptor

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// TestRingBuffer_BasicOperations tests basic ring buffer operations
func TestRingBuffer_BasicOperations(t *testing.T) {
	rb := NewRingBuffer(DefaultConfig())

	if rb.GetBufferSize() != 8192 {
		t.Errorf("Expected buffer size 8192, got %d", rb.GetBufferSize())
	}

	// Test that buffer size is power of 2
	size := rb.bufferSize
	if size&(size-1) != 0 {
		t.Errorf("Buffer size %d is not a power of 2", size)
	}

	// Test index mask
	expectedMask := size - 1
	if rb.indexMask != expectedMask {
		t.Errorf("Expected index mask %d, got %d", expectedMask, rb.indexMask)
	}
}

// TestSequencer_SingleProducer tests single producer scenario
func TestSequencer_SingleProducer(t *testing.T) {
	rb := NewRingBuffer(Config{BufferSize: 1024})
	seq := NewSequencer(rb)

	// Claim 100 sequences
	for i := uint64(1); i <= 100; i++ {
		s, err := seq.Next()
		if err != nil {
			t.Fatalf("Failed to claim sequence %d: %v", i, err)
		}
		if s != i {
			t.Errorf("Expected sequence %d, got %d", i, s)
		}
	}
}

// TestSequencer_MultiProducer tests concurrent producers
func TestSequencer_MultiProducer(t *testing.T) {
	rb := NewRingBuffer(Config{BufferSize: 4096})
	seq := NewSequencer(rb)

	numProducers := 10
	sequencesPerProducer := 100

	var wg sync.WaitGroup
	claimed := make(map[uint64]bool)
	claimedMu := sync.Mutex{}

	wg.Add(numProducers)

	for p := 0; p < numProducers; p++ {
		go func() {
			defer wg.Done()

			for i := 0; i < sequencesPerProducer; i++ {
				s, err := seq.Next()
				if err != nil {
					t.Errorf("Failed to claim sequence: %v", err)
					return
				}

				// Check for duplicates
				claimedMu.Lock()
				if claimed[s] {
					t.Errorf("Duplicate sequence claimed: %d", s)
				}
				claimed[s] = true
				claimedMu.Unlock()
			}
		}()
	}

	wg.Wait()

	// Verify all sequences were claimed exactly once
	expectedTotal := numProducers * sequencesPerProducer
	if len(claimed) != expectedTotal {
		t.Errorf("Expected %d unique sequences, got %d", expectedTotal, len(claimed))
	}
}

// TestSequencer_Backpressure tests backpressure when buffer fills
func TestSequencer_Backpressure(t *testing.T) {
	rb := NewRingBuffer(Config{BufferSize: 16}) // Small buffer
	seq := NewSequencer(rb)

	// Fill the buffer completely
	for i := uint64(1); i <= 16; i++ {
		s, err := seq.Next()
		if err != nil {
			t.Fatalf("Failed to claim sequence %d: %v", i, err)
		}
		// Don't publish - keep slots claimed
		_ = s
	}

	// Try to claim one more - should fail with backpressure
	_, err := seq.Next()
	if err != ErrBufferFull {
		t.Errorf("Expected ErrBufferFull, got %v", err)
	}
}

// TestDisruptorIntegration tests the full disruptor flow
func TestDisruptorIntegration(t *testing.T) {
	// This test would require a full engine setup
	// For now, we'll test the basic publish/consume flow

	rb := NewRingBuffer(Config{BufferSize: 1024})
	seq := NewSequencer(rb)

	// Track consumed sequences
	var consumed uint64

	// Producer: claim and publish
	numOrders := 100
	responseChs := make([]chan *OrderResponse, numOrders)

	for i := 0; i < numOrders; i++ {
		s, err := seq.Next()
		if err != nil {
			t.Fatalf("Failed to claim sequence: %v", err)
		}

		responseChs[i] = make(chan *OrderResponse, 1)

		request := &OrderRequest{
			Type: RequestTypeNewOrder,
			Order: &orders.Order{
				Symbol:   "AAPL",
				Side:     orders.SideBuy,
				Type:     orders.OrderTypeLimit,
				Price:    150000, // $150.00
				Quantity: 100,
			},
		}

		seq.Publish(s, request, responseChs[i])
	}

	// Consumer: read from ring buffer
	nextSeq := uint64(1)
	for nextSeq <= uint64(numOrders) {
		index := nextSeq & rb.indexMask
		slot := &rb.slots[index]

		// Wait for slot to be ready
		for {
			available := atomic.LoadUint64(&slot.SequenceNum)
			if available == nextSeq {
				break
			}
			time.Sleep(10 * time.Microsecond)
		}

		// Verify request
		if slot.Request == nil {
			t.Errorf("Slot %d has nil request", nextSeq)
		}
		if slot.Request.Type != RequestTypeNewOrder {
			t.Errorf("Expected RequestTypeNewOrder, got %d", slot.Request.Type)
		}
		if slot.Request.Order.Symbol != "AAPL" {
			t.Errorf("Expected symbol AAPL, got %s", slot.Request.Order.Symbol)
		}

		// Update gating sequence
		atomic.StoreUint64(&rb.gatingSequence, nextSeq)

		nextSeq++
		consumed++
	}

	if consumed != uint64(numOrders) {
		t.Errorf("Expected to consume %d orders, consumed %d", numOrders, consumed)
	}
}

// BenchmarkSequencer_SingleProducer benchmarks single producer throughput
func BenchmarkSequencer_SingleProducer(b *testing.B) {
	rb := NewRingBuffer(Config{BufferSize: 8192})
	seq := NewSequencer(rb)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s, err := seq.Next()
		if err != nil {
			b.Fatalf("Failed to claim sequence: %v", err)
		}

		// Simulate publish
		index := s & rb.indexMask
		atomic.StoreUint64(&rb.slots[index].SequenceNum, s)

		// Update gating to allow reuse
		if i%100 == 0 {
			atomic.StoreUint64(&rb.gatingSequence, s-rb.bufferSize/2)
		}
	}
}

// BenchmarkSequencer_MultiProducer benchmarks multi-producer throughput
func BenchmarkSequencer_MultiProducer(b *testing.B) {
	rb := NewRingBuffer(Config{BufferSize: 8192})
	seq := NewSequencer(rb)

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s, err := seq.Next()
			if err != nil {
				continue // Skip on backpressure
			}

			// Simulate publish
			index := s & rb.indexMask
			atomic.StoreUint64(&rb.slots[index].SequenceNum, s)
		}
	})
}
