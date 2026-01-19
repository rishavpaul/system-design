// Package marketdata handles real-time market data distribution.
//
// Market Data Levels:
//
// L1 (Level 1) - Top of Book:
//   - Best bid price and size
//   - Best ask price and size
//   - Last trade price and size
//   - Used by: Retail traders, basic displays
//
// L2 (Level 2) - Depth:
//   - Multiple price levels (typically top 5-10)
//   - Total size at each level
//   - Used by: Active traders, algorithms
//
// L3 (Level 3) - Full Order Book:
//   - Every individual order
//   - Rarely available to public
//   - Used by: Market makers, exchanges
//
// Distribution Patterns:
// - Multicast: Efficient for many subscribers (UDP multicast)
// - WebSocket: For web clients
// - FIX Protocol: Industry standard for institutions
package marketdata

import (
	"sync"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// L1Quote represents Level 1 (top of book) market data.
type L1Quote struct {
	Symbol    string
	BidPrice  int64
	BidSize   int64
	AskPrice  int64
	AskSize   int64
	LastPrice int64
	LastSize  int64
	Timestamp int64
}

// L2Depth represents Level 2 (depth) market data.
type L2Depth struct {
	Symbol    string
	Bids      []PriceLevel
	Asks      []PriceLevel
	Timestamp int64
}

// PriceLevel represents a single price level in depth data.
type PriceLevel struct {
	Price    int64
	Quantity int64
	Count    int // Number of orders at this level
}

// TradeReport represents a trade execution report.
type TradeReport struct {
	TradeID       uint64
	Symbol        string
	Price         int64
	Quantity      int64
	AggressorSide orders.Side // Which side initiated the trade
	Timestamp     int64
}

// Publisher distributes market data to subscribers.
type Publisher struct {
	mu          sync.RWMutex
	l1Subs      map[string][]chan L1Quote
	l2Subs      map[string][]chan L2Depth
	tradeSubs   map[string][]chan TradeReport
	allL1Subs   []chan L1Quote    // Subscribers to all symbols
	allTradeSubs []chan TradeReport // Subscribers to all trades
	bufferSize  int
}

// NewPublisher creates a new market data publisher.
func NewPublisher(bufferSize int) *Publisher {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Publisher{
		l1Subs:     make(map[string][]chan L1Quote),
		l2Subs:     make(map[string][]chan L2Depth),
		tradeSubs:  make(map[string][]chan TradeReport),
		bufferSize: bufferSize,
	}
}

// SubscribeL1 subscribes to L1 quotes for a symbol.
// Returns a channel that will receive updates.
func (p *Publisher) SubscribeL1(symbol string) <-chan L1Quote {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan L1Quote, p.bufferSize)
	p.l1Subs[symbol] = append(p.l1Subs[symbol], ch)
	return ch
}

// SubscribeAllL1 subscribes to L1 quotes for all symbols.
func (p *Publisher) SubscribeAllL1() <-chan L1Quote {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan L1Quote, p.bufferSize)
	p.allL1Subs = append(p.allL1Subs, ch)
	return ch
}

// SubscribeL2 subscribes to L2 depth for a symbol.
func (p *Publisher) SubscribeL2(symbol string) <-chan L2Depth {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan L2Depth, p.bufferSize)
	p.l2Subs[symbol] = append(p.l2Subs[symbol], ch)
	return ch
}

// SubscribeTrades subscribes to trade reports for a symbol.
func (p *Publisher) SubscribeTrades(symbol string) <-chan TradeReport {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan TradeReport, p.bufferSize)
	p.tradeSubs[symbol] = append(p.tradeSubs[symbol], ch)
	return ch
}

// SubscribeAllTrades subscribes to trade reports for all symbols.
func (p *Publisher) SubscribeAllTrades() <-chan TradeReport {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan TradeReport, p.bufferSize)
	p.allTradeSubs = append(p.allTradeSubs, ch)
	return ch
}

// PublishL1 sends an L1 quote update to subscribers.
// Non-blocking: drops updates if subscriber channel is full.
func (p *Publisher) PublishL1(quote L1Quote) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Send to symbol-specific subscribers
	for _, ch := range p.l1Subs[quote.Symbol] {
		select {
		case ch <- quote:
		default:
			// Channel full, drop update (subscriber is slow)
		}
	}

	// Send to all-symbols subscribers
	for _, ch := range p.allL1Subs {
		select {
		case ch <- quote:
		default:
		}
	}
}

// PublishL2 sends an L2 depth update to subscribers.
func (p *Publisher) PublishL2(depth L2Depth) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, ch := range p.l2Subs[depth.Symbol] {
		select {
		case ch <- depth:
		default:
		}
	}
}

// PublishTrade sends a trade report to subscribers.
func (p *Publisher) PublishTrade(trade TradeReport) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Send to symbol-specific subscribers
	for _, ch := range p.tradeSubs[trade.Symbol] {
		select {
		case ch <- trade:
		default:
		}
	}

	// Send to all-trades subscribers
	for _, ch := range p.allTradeSubs {
		select {
		case ch <- trade:
		default:
		}
	}
}

// Unsubscribe removes a subscription channel.
// Note: In production, we'd track subscription IDs for clean removal.
func (p *Publisher) UnsubscribeL1(symbol string, ch <-chan L1Quote) {
	p.mu.Lock()
	defer p.mu.Unlock()

	subs := p.l1Subs[symbol]
	for i, sub := range subs {
		if sub == ch {
			p.l1Subs[symbol] = append(subs[:i], subs[i+1:]...)
			close(sub)
			return
		}
	}
}

// Close closes all subscription channels.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, subs := range p.l1Subs {
		for _, ch := range subs {
			close(ch)
		}
	}
	for _, subs := range p.l2Subs {
		for _, ch := range subs {
			close(ch)
		}
	}
	for _, subs := range p.tradeSubs {
		for _, ch := range subs {
			close(ch)
		}
	}
	for _, ch := range p.allL1Subs {
		close(ch)
	}
	for _, ch := range p.allTradeSubs {
		close(ch)
	}
}
