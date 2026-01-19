package disruptor

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"

	"github.com/rishav/order-matching-engine/internal/events"
	"github.com/rishav/order-matching-engine/internal/matching"
	"github.com/rishav/order-matching-engine/internal/orders"
)

// EventProcessor processes orders from the ring buffer in a single thread.
//
// Design:
// - Single goroutine for deterministic, sequential processing
// - Reads from ring buffer using spin-wait
// - Calls matching engine (single-threaded, no locks needed)
// - Queues events for batched async logging
// - Sends responses back to HTTP handlers via channels
type EventProcessor struct {
	rb           *RingBuffer
	engine       *matching.Engine
	eventBatcher *EventBatcher
	running      atomic.Bool
	shutdownCh   chan struct{}
	shutdownDone chan struct{}
}

// NewEventProcessor creates a new event processor.
func NewEventProcessor(rb *RingBuffer, engine *matching.Engine, eventLog *events.EventLog) *EventProcessor {
	return &EventProcessor{
		rb:           rb,
		engine:       engine,
		eventBatcher: NewEventBatcher(eventLog, 1000, 10), // 1000 events or 10ms
		shutdownCh:   make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
}

// Start begins processing events from the ring buffer.
func (p *EventProcessor) Start() {
	p.running.Store(true)
	go p.processLoop()
	go p.eventBatcher.Start()
}

// processLoop is the main event processing loop (single goroutine).
//
// This loop maintains determinism by processing orders sequentially
// in sequence number order. It never uses locks, relying on the
// single-threaded nature for correctness.
func (p *EventProcessor) processLoop() {
	defer close(p.shutdownDone)

	nextSequence := uint64(1) // Start at 1 (0 is initial state)

	for p.running.Load() {
		// Calculate slot index
		index := nextSequence & p.rb.indexMask
		slot := &p.rb.slots[index]

		// Spin-wait for publisher to finish writing
		// The slot is ready when its SequenceNum matches our expected sequence
		for {
			available := atomic.LoadUint64(&slot.SequenceNum)
			if available == nextSequence {
				break
			}

			// Check for shutdown signal
			select {
			case <-p.shutdownCh:
				return
			default:
				// Yield to other goroutines to avoid busy loop
				runtime.Gosched()
			}
		}

		// Process the request
		p.processRequest(slot)

		// Update gating sequence to allow this slot to be reused
		atomic.StoreUint64(&p.rb.gatingSequence, nextSequence)

		nextSequence++
	}
}

// processRequest processes a single request from the ring buffer.
func (p *EventProcessor) processRequest(slot *RingBufferSlot) {
	req := slot.Request
	responseCh := slot.ResponseCh

	// Panic recovery to prevent processor crash
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR: Event processor panic: %v", r)
			// Send error response
			select {
			case responseCh <- &OrderResponse{
				Success: false,
				Error:   fmt.Errorf("internal error: %v", r),
			}:
			default:
			}
		}
	}()

	// Route based on request type
	switch req.Type {
	case RequestTypeNewOrder:
		p.processNewOrder(req, responseCh)
	case RequestTypeCancelOrder:
		p.processCancelOrder(req, responseCh)
	default:
		// Unknown request type
		select {
		case responseCh <- &OrderResponse{
			Success: false,
			Error:   fmt.Errorf("unknown request type: %d", req.Type),
		}:
		default:
		}
	}
}

// processNewOrder processes a new order submission.
func (p *EventProcessor) processNewOrder(req *OrderRequest, responseCh chan *OrderResponse) {
	order := req.Order

	// Process order through matching engine (single-threaded, deterministic)
	result := p.engine.ProcessOrder(order)

	// Queue events for batched logging
	if result.Accepted {
		// Log new order event
		p.eventBatcher.QueueEvent(&events.NewOrderEvent{
			Event: events.Event{
				Timestamp: orders.Now(),
				Type:      events.EventTypeNewOrder,
			},
			OrderID:   order.ID,
			Symbol:    order.Symbol,
			Side:      order.Side,
			OrderType: order.Type,
			Price:     order.Price,
			Quantity:  order.Quantity,
			AccountID: order.AccountID,
		})

		// Log fill events
		for _, fill := range result.Fills {
			p.eventBatcher.QueueEvent(&events.FillEvent{
				Event: events.Event{
					Timestamp: orders.Now(),
					Type:      events.EventTypeFill,
				},
				TradeID:        fill.TradeID,
				Symbol:         fill.Symbol,
				Price:          fill.Price,
				Quantity:       fill.Quantity,
				MakerOrderID:   fill.MakerOrderID,
				TakerOrderID:   fill.TakerOrderID,
				MakerAccountID: fill.MakerAccountID,
				TakerAccountID: fill.TakerAccountID,
				TakerSide:      fill.TakerSide,
			})
		}
	}

	// Send response back to HTTP handler
	select {
	case responseCh <- &OrderResponse{
		Success: result.Accepted,
		Result:  result,
		Order:   order,
	}:
	default:
		// Handler timed out or channel closed, drop response
		log.Printf("Warning: Failed to send order response for order %d", order.ID)
	}
}

// processCancelOrder processes an order cancellation.
func (p *EventProcessor) processCancelOrder(req *OrderRequest, responseCh chan *OrderResponse) {
	// Cancel the order
	order, err := p.engine.CancelOrder(req.Symbol, req.OrderID)

	// Queue cancellation event if successful
	if err == nil && order != nil {
		p.eventBatcher.QueueEvent(&events.OrderCancelledEvent{
			Event: events.Event{
				Timestamp: orders.Now(),
				Type:      events.EventTypeOrderCancelled,
			},
			OrderID:      order.ID,
			Symbol:       order.Symbol,
			CancelledQty: order.RemainingQty(),
			Reason:       "user cancelled",
		})
	}

	// Send response
	select {
	case responseCh <- &OrderResponse{
		Success: err == nil,
		Order:   order,
		Error:   err,
	}:
	default:
		log.Printf("Warning: Failed to send cancel response for order %d", req.OrderID)
	}
}

// Shutdown gracefully shuts down the event processor.
//
// It stops accepting new requests, drains remaining requests from the ring buffer,
// and ensures all events are flushed to the event log.
func (p *EventProcessor) Shutdown() {
	log.Println("Shutting down event processor...")

	// Signal shutdown
	p.running.Store(false)
	close(p.shutdownCh)

	// Wait for processor loop to finish
	<-p.shutdownDone

	// Shutdown event batcher (flushes remaining events)
	p.eventBatcher.Shutdown()

	log.Println("Event processor shutdown complete")
}
