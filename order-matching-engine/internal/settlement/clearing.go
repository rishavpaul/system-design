// Package settlement simulates the clearing and settlement process.
//
// Trade Lifecycle:
//
// T+0 (Trade Date):
//   - Order matched → Trade executed
//   - Trade reported to clearing house
//   - Both parties notified
//
// T+1 (Trade Date + 1):
//   - Clearing house calculates obligations
//   - Netting: Reduce multiple trades to net positions
//   - Margin verification
//   - Generate settlement instructions
//
// T+2 (Settlement Date):
//   - Delivery vs Payment (DVP): Securities and cash exchanged atomically
//   - Final settlement
//   - Positions updated
//
// Why T+2?
// - Historically T+5 (paper certificates), then T+3, now T+2
// - US moving to T+1 in 2024
// - Gives time to arrange financing, locate securities
// - Risk: Counterparty might fail before settlement
//
// Netting Example:
//
//	Without netting:
//	  Trade 1: A buys 100 AAPL from B @ $150
//	  Trade 2: A sells 60 AAPL to B @ $151
//	  Trade 3: A buys 40 AAPL from B @ $149
//	  = 3 settlements, 180 shares moved
//
//	With netting:
//	  Net: A buys 80 AAPL from B @ weighted avg price
//	  = 1 settlement, 80 shares moved (55% reduction!)
package settlement

import (
	"fmt"
	"sync"
	"time"

	"github.com/rishav/order-matching-engine/internal/orders"
)

// TradeStatus represents the settlement status of a trade.
type TradeStatus int

const (
	TradeStatusExecuted TradeStatus = iota
	TradeStatusClearing
	TradeStatusReadyToSettle
	TradeStatusSettled
	TradeStatusFailed
)

func (s TradeStatus) String() string {
	switch s {
	case TradeStatusExecuted:
		return "EXECUTED"
	case TradeStatusClearing:
		return "CLEARING"
	case TradeStatusReadyToSettle:
		return "READY_TO_SETTLE"
	case TradeStatusSettled:
		return "SETTLED"
	case TradeStatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// Trade represents a trade pending settlement.
type Trade struct {
	ID            uint64
	Symbol        string
	Price         int64
	Quantity      int64
	BuyerAccount  string
	SellerAccount string
	TradeTime     time.Time
	SettleDate    time.Time
	Status        TradeStatus
}

// NetPosition represents a netted position for an account/symbol pair.
type NetPosition struct {
	AccountID string
	Symbol    string
	NetQty    int64 // Positive = long (owes delivery), Negative = short (receives)
	NetValue  int64 // Net cash value (positive = owes cash)
}

// SettlementInstruction represents what needs to happen at settlement.
type SettlementInstruction struct {
	TradeIDs     []uint64 // Trades included in this settlement
	FromAccount  string
	ToAccount    string
	Symbol       string
	Quantity     int64
	CashAmount   int64 // In cents
	SettleDate   time.Time
	Status       TradeStatus
}

// Account represents an account's balances.
type Account struct {
	ID       string
	Cash     int64            // Cash balance in cents
	Holdings map[string]int64 // symbol -> quantity
}

// ClearingHouse manages the clearing and settlement process.
type ClearingHouse struct {
	trades       map[uint64]*Trade
	accounts     map[string]*Account
	instructions []SettlementInstruction
	mu           sync.RWMutex
	settlementDays int // T+N settlement (default 2)
}

// NewClearingHouse creates a new clearing house.
func NewClearingHouse() *ClearingHouse {
	return &ClearingHouse{
		trades:         make(map[uint64]*Trade),
		accounts:       make(map[string]*Account),
		settlementDays: 2,
	}
}

// GetOrCreateAccount gets or creates an account.
func (ch *ClearingHouse) GetOrCreateAccount(accountID string, initialCash int64) *Account {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	if acct, exists := ch.accounts[accountID]; exists {
		return acct
	}

	acct := &Account{
		ID:       accountID,
		Cash:     initialCash,
		Holdings: make(map[string]int64),
	}
	ch.accounts[accountID] = acct
	return acct
}

// GetAccount retrieves an account.
func (ch *ClearingHouse) GetAccount(accountID string) *Account {
	ch.mu.RLock()
	defer ch.mu.RUnlock()
	return ch.accounts[accountID]
}

// RecordTrade records a new trade for settlement.
func (ch *ClearingHouse) RecordTrade(fill orders.Fill) *Trade {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	now := time.Now()
	settleDate := ch.calculateSettleDate(now)

	var buyerAccount, sellerAccount string
	if fill.TakerSide == orders.SideBuy {
		buyerAccount = fill.TakerAccountID
		sellerAccount = fill.MakerAccountID
	} else {
		buyerAccount = fill.MakerAccountID
		sellerAccount = fill.TakerAccountID
	}

	trade := &Trade{
		ID:            fill.TradeID,
		Symbol:        fill.Symbol,
		Price:         fill.Price,
		Quantity:      fill.Quantity,
		BuyerAccount:  buyerAccount,
		SellerAccount: sellerAccount,
		TradeTime:     now,
		SettleDate:    settleDate,
		Status:        TradeStatusExecuted,
	}

	ch.trades[trade.ID] = trade
	return trade
}

// calculateSettleDate calculates T+N settlement date.
func (ch *ClearingHouse) calculateSettleDate(tradeDate time.Time) time.Time {
	settleDate := tradeDate
	daysAdded := 0

	for daysAdded < ch.settlementDays {
		settleDate = settleDate.AddDate(0, 0, 1)
		// Skip weekends
		if settleDate.Weekday() != time.Saturday && settleDate.Weekday() != time.Sunday {
			daysAdded++
		}
	}

	return settleDate
}

// CalculateNetting calculates net positions for all pending trades.
// This reduces the number of actual transfers needed.
func (ch *ClearingHouse) CalculateNetting() map[string]map[string]NetPosition {
	ch.mu.RLock()
	defer ch.mu.RUnlock()
	return ch.calculateNettingLocked()
}

// calculateNettingLocked is the internal version that assumes the caller holds a lock.
func (ch *ClearingHouse) calculateNettingLocked() map[string]map[string]NetPosition {
	// account -> symbol -> NetPosition
	netPositions := make(map[string]map[string]NetPosition)

	for _, trade := range ch.trades {
		if trade.Status != TradeStatusExecuted && trade.Status != TradeStatusClearing {
			continue
		}

		tradeValue := trade.Price * trade.Quantity

		// Buyer: receives shares, owes cash
		if netPositions[trade.BuyerAccount] == nil {
			netPositions[trade.BuyerAccount] = make(map[string]NetPosition)
		}
		buyerPos := netPositions[trade.BuyerAccount][trade.Symbol]
		buyerPos.AccountID = trade.BuyerAccount
		buyerPos.Symbol = trade.Symbol
		buyerPos.NetQty += trade.Quantity  // Will receive shares
		buyerPos.NetValue += tradeValue    // Owes cash
		netPositions[trade.BuyerAccount][trade.Symbol] = buyerPos

		// Seller: delivers shares, receives cash
		if netPositions[trade.SellerAccount] == nil {
			netPositions[trade.SellerAccount] = make(map[string]NetPosition)
		}
		sellerPos := netPositions[trade.SellerAccount][trade.Symbol]
		sellerPos.AccountID = trade.SellerAccount
		sellerPos.Symbol = trade.Symbol
		sellerPos.NetQty -= trade.Quantity  // Will deliver shares
		sellerPos.NetValue -= tradeValue    // Will receive cash
		netPositions[trade.SellerAccount][trade.Symbol] = sellerPos
	}

	return netPositions
}

// GenerateSettlementInstructions creates settlement instructions from netted positions.
func (ch *ClearingHouse) GenerateSettlementInstructions() []SettlementInstruction {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	netPositions := ch.calculateNettingLocked()
	var instructions []SettlementInstruction

	// For each symbol, match buyers and sellers
	symbolNets := make(map[string][]NetPosition)
	for _, positions := range netPositions {
		for _, pos := range positions {
			symbolNets[pos.Symbol] = append(symbolNets[pos.Symbol], pos)
		}
	}

	for symbol, positions := range symbolNets {
		// Separate longs (receivers) and shorts (deliverers)
		var receivers, deliverers []NetPosition
		for _, pos := range positions {
			if pos.NetQty > 0 {
				receivers = append(receivers, pos)
			} else if pos.NetQty < 0 {
				deliverers = append(deliverers, pos)
			}
		}

		// Match deliverers to receivers
		for _, deliverer := range deliverers {
			qtyToDeliver := -deliverer.NetQty

			for i := range receivers {
				if qtyToDeliver <= 0 {
					break
				}
				if receivers[i].NetQty <= 0 {
					continue
				}

				matchQty := min64(qtyToDeliver, receivers[i].NetQty)
				avgPrice := deliverer.NetValue / deliverer.NetQty
				cashAmount := matchQty * avgPrice

				instruction := SettlementInstruction{
					FromAccount: deliverer.AccountID,
					ToAccount:   receivers[i].AccountID,
					Symbol:      symbol,
					Quantity:    matchQty,
					CashAmount:  -cashAmount, // Negative because deliverer receives cash
					SettleDate:  time.Now().AddDate(0, 0, ch.settlementDays),
					Status:      TradeStatusReadyToSettle,
				}
				instructions = append(instructions, instruction)

				qtyToDeliver -= matchQty
				receivers[i].NetQty -= matchQty
			}
		}
	}

	ch.instructions = instructions
	return instructions
}

// Settle executes settlement for all ready instructions.
func (ch *ClearingHouse) Settle() ([]SettlementInstruction, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	var settled []SettlementInstruction
	var errors []string

	for i := range ch.instructions {
		instr := &ch.instructions[i]
		if instr.Status != TradeStatusReadyToSettle {
			continue
		}

		// Get accounts
		fromAcct := ch.accounts[instr.FromAccount]
		toAcct := ch.accounts[instr.ToAccount]

		if fromAcct == nil || toAcct == nil {
			instr.Status = TradeStatusFailed
			errors = append(errors, fmt.Sprintf("account not found for instruction %s->%s",
				instr.FromAccount, instr.ToAccount))
			continue
		}

		// Check deliverer has sufficient shares
		if fromAcct.Holdings[instr.Symbol] < instr.Quantity {
			instr.Status = TradeStatusFailed
			errors = append(errors, fmt.Sprintf("insufficient shares: %s has %d, needs %d",
				instr.FromAccount, fromAcct.Holdings[instr.Symbol], instr.Quantity))
			continue
		}

		// Check receiver has sufficient cash
		if toAcct.Cash < instr.CashAmount {
			instr.Status = TradeStatusFailed
			errors = append(errors, fmt.Sprintf("insufficient cash: %s has %s, needs %s",
				instr.ToAccount, orders.FormatPrice(toAcct.Cash), orders.FormatPrice(instr.CashAmount)))
			continue
		}

		// Execute DVP (Delivery vs Payment) atomically
		// Shares: From deliverer to receiver
		fromAcct.Holdings[instr.Symbol] -= instr.Quantity
		toAcct.Holdings[instr.Symbol] += instr.Quantity

		// Cash: From receiver to deliverer
		toAcct.Cash -= instr.CashAmount
		fromAcct.Cash += instr.CashAmount

		instr.Status = TradeStatusSettled
		settled = append(settled, *instr)
	}

	// Update trade statuses
	for _, trade := range ch.trades {
		if trade.Status == TradeStatusClearing || trade.Status == TradeStatusReadyToSettle {
			trade.Status = TradeStatusSettled
		}
	}

	if len(errors) > 0 {
		return settled, fmt.Errorf("settlement errors: %v", errors)
	}

	return settled, nil
}

// GetPendingTrades returns all trades pending settlement.
func (ch *ClearingHouse) GetPendingTrades() []*Trade {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	var pending []*Trade
	for _, trade := range ch.trades {
		if trade.Status != TradeStatusSettled && trade.Status != TradeStatusFailed {
			pending = append(pending, trade)
		}
	}
	return pending
}

// GetSettlementStats returns statistics about the settlement process.
func (ch *ClearingHouse) GetSettlementStats() map[string]int {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	stats := map[string]int{
		"total_trades":   len(ch.trades),
		"executed":       0,
		"clearing":       0,
		"ready":          0,
		"settled":        0,
		"failed":         0,
		"instructions":   len(ch.instructions),
	}

	for _, trade := range ch.trades {
		switch trade.Status {
		case TradeStatusExecuted:
			stats["executed"]++
		case TradeStatusClearing:
			stats["clearing"]++
		case TradeStatusReadyToSettle:
			stats["ready"]++
		case TradeStatusSettled:
			stats["settled"]++
		case TradeStatusFailed:
			stats["failed"]++
		}
	}

	return stats
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
