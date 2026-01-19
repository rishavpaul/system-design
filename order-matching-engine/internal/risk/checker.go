// Package risk implements pre-trade risk checks.
//
// Pre-trade risk checks are critical for:
// 1. Protecting the exchange from bad actors
// 2. Protecting traders from their own mistakes (fat finger errors)
// 3. Ensuring orderly markets
// 4. Regulatory compliance
//
// Checks are performed BEFORE the order reaches the matching engine.
// They can run in parallel since they don't modify order book state.
//
// Common Risk Controls:
// - Order size limits (max shares per order)
// - Order value limits (max dollar value per order)
// - Price bands (reject orders too far from market)
// - Position limits (max shares held)
// - Daily volume limits (max traded per day)
// - Rate limits (max orders per second)
package risk

import (
	"fmt"
	"sync"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// CheckResult contains the result of a risk check.
type CheckResult struct {
	Passed    bool
	Reason    string   // If failed, why
	ChecksRun []string // List of checks that were run
}

// Config configures the risk checker.
type Config struct {
	MaxOrderSize     int64            // Maximum shares per order
	MaxOrderValue    int64            // Maximum dollar value per order (in cents)
	MaxPositionSize  int64            // Maximum position size per symbol
	MaxDailyVolume   int64            // Maximum daily trading volume per account (in cents)
	PriceBandPercent float64          // Max deviation from reference price (0.1 = 10%)
	SymbolLimits     map[string]int64 // Per-symbol position limits
}

// DefaultConfig returns a reasonable default configuration.
func DefaultConfig() Config {
	return Config{
		MaxOrderSize:     100000,    // 100,000 shares
		MaxOrderValue:    10000000,  // $100,000
		MaxPositionSize:  1000000,   // 1,000,000 shares
		MaxDailyVolume:   100000000, // $1,000,000 daily
		PriceBandPercent: 0.10,      // 10% from reference price
	}
}

// Checker performs pre-trade risk checks.
type Checker struct {
	config         Config
	positions      map[string]map[string]int64 // account -> symbol -> position
	dailyVolume    map[string]int64            // account -> daily volume (in cents)
	referencePrices map[string]int64           // symbol -> last known price
	mu             sync.RWMutex
}

// NewChecker creates a new risk checker.
func NewChecker(config Config) *Checker {
	return &Checker{
		config:          config,
		positions:       make(map[string]map[string]int64),
		dailyVolume:     make(map[string]int64),
		referencePrices: make(map[string]int64),
	}
}

// Check performs all risk checks on an order.
// Returns immediately on first failure.
func (c *Checker) Check(order *orders.Order) CheckResult {
	result := CheckResult{
		Passed:    true,
		ChecksRun: make([]string, 0),
	}

	// 1. Order size check
	result.ChecksRun = append(result.ChecksRun, "order_size")
	if order.Quantity > c.config.MaxOrderSize {
		return CheckResult{
			Passed:    false,
			Reason:    fmt.Sprintf("order size %d exceeds max %d", order.Quantity, c.config.MaxOrderSize),
			ChecksRun: result.ChecksRun,
		}
	}

	// 2. Order value check (skip for market orders without price)
	if order.Price > 0 {
		result.ChecksRun = append(result.ChecksRun, "order_value")
		orderValue := order.Price * order.Quantity
		if orderValue > c.config.MaxOrderValue {
			return CheckResult{
				Passed:    false,
				Reason:    fmt.Sprintf("order value %s exceeds max %s", orders.FormatPrice(orderValue), orders.FormatPrice(c.config.MaxOrderValue)),
				ChecksRun: result.ChecksRun,
			}
		}
	}

	// 3. Price band check (for limit orders)
	if order.Type == orders.OrderTypeLimit && order.Price > 0 {
		result.ChecksRun = append(result.ChecksRun, "price_band")
		if !c.checkPriceBand(order) {
			refPrice := c.GetReferencePrice(order.Symbol)
			return CheckResult{
				Passed: false,
				Reason: fmt.Sprintf("price %s outside band (ref: %s, band: %.0f%%)",
					orders.FormatPrice(order.Price),
					orders.FormatPrice(refPrice),
					c.config.PriceBandPercent*100),
				ChecksRun: result.ChecksRun,
			}
		}
	}

	// 4. Position limit check
	result.ChecksRun = append(result.ChecksRun, "position_limit")
	if !c.checkPositionLimit(order) {
		currentPos := c.GetPosition(order.AccountID, order.Symbol)
		return CheckResult{
			Passed:    false,
			Reason:    fmt.Sprintf("would exceed position limit (current: %d, order: %d, max: %d)", currentPos, order.Quantity, c.config.MaxPositionSize),
			ChecksRun: result.ChecksRun,
		}
	}

	// 5. Daily volume check
	if order.Price > 0 {
		result.ChecksRun = append(result.ChecksRun, "daily_volume")
		orderValue := order.Price * order.Quantity
		if !c.checkDailyVolume(order.AccountID, orderValue) {
			currentVol := c.GetDailyVolume(order.AccountID)
			return CheckResult{
				Passed:    false,
				Reason:    fmt.Sprintf("would exceed daily volume limit (current: %s, order: %s, max: %s)", orders.FormatPrice(currentVol), orders.FormatPrice(orderValue), orders.FormatPrice(c.config.MaxDailyVolume)),
				ChecksRun: result.ChecksRun,
			}
		}
	}

	return result
}

// checkPriceBand verifies the order price is within acceptable range.
func (c *Checker) checkPriceBand(order *orders.Order) bool {
	c.mu.RLock()
	refPrice, exists := c.referencePrices[order.Symbol]
	c.mu.RUnlock()

	if !exists || refPrice == 0 {
		return true // No reference price, allow order
	}

	band := float64(refPrice) * c.config.PriceBandPercent
	lowBound := refPrice - int64(band)
	highBound := refPrice + int64(band)

	return order.Price >= lowBound && order.Price <= highBound
}

// checkPositionLimit verifies the order won't exceed position limits.
func (c *Checker) checkPositionLimit(order *orders.Order) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	currentPos := int64(0)
	if acct, exists := c.positions[order.AccountID]; exists {
		currentPos = acct[order.Symbol]
	}

	// Calculate projected position
	var projectedPos int64
	if order.Side == orders.SideBuy {
		projectedPos = currentPos + order.Quantity
	} else {
		projectedPos = currentPos - order.Quantity
	}

	// Check against limit (absolute value)
	limit := c.config.MaxPositionSize
	if symLimit, exists := c.config.SymbolLimits[order.Symbol]; exists {
		limit = symLimit
	}

	if projectedPos < 0 {
		projectedPos = -projectedPos
	}
	return projectedPos <= limit
}

// checkDailyVolume verifies the order won't exceed daily volume limits.
func (c *Checker) checkDailyVolume(accountID string, orderValue int64) bool {
	c.mu.RLock()
	currentVolume := c.dailyVolume[accountID]
	c.mu.RUnlock()

	return currentVolume+orderValue <= c.config.MaxDailyVolume
}

// UpdatePosition updates the position for an account after a fill.
func (c *Checker) UpdatePosition(accountID, symbol string, side orders.Side, quantity int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.positions[accountID] == nil {
		c.positions[accountID] = make(map[string]int64)
	}

	if side == orders.SideBuy {
		c.positions[accountID][symbol] += quantity
	} else {
		c.positions[accountID][symbol] -= quantity
	}
}

// UpdateDailyVolume updates the daily volume for an account after a fill.
func (c *Checker) UpdateDailyVolume(accountID string, value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dailyVolume[accountID] += value
}

// SetReferencePrice sets the reference price for a symbol.
// Called after each trade to update the last traded price.
func (c *Checker) SetReferencePrice(symbol string, price int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.referencePrices[symbol] = price
}

// GetReferencePrice returns the current reference price for a symbol.
func (c *Checker) GetReferencePrice(symbol string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.referencePrices[symbol]
}

// GetPosition returns the current position for an account and symbol.
func (c *Checker) GetPosition(accountID, symbol string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if acct, exists := c.positions[accountID]; exists {
		return acct[symbol]
	}
	return 0
}

// GetDailyVolume returns the current daily volume for an account.
func (c *Checker) GetDailyVolume(accountID string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dailyVolume[accountID]
}

// ResetDailyVolume resets daily volume counters (called at start of trading day).
func (c *Checker) ResetDailyVolume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dailyVolume = make(map[string]int64)
}
