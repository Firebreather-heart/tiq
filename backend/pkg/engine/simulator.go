package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"tiq/backend/pkg/db"
	"tiq/backend/pkg/oanda"
)

type Simulator struct {
	store     *db.DB
	accountID string
	prices    map[string]float64
	mu        sync.RWMutex
}

func NewSimulator(store *db.DB, initialBalance float64) (*Simulator, error) {
	accID := "local_simulator"
	_, err := store.GetAccount(accID)
	if err != nil {
		// Create the default account if it doesn't exist
		err = store.SaveAccount(db.Account{
			ID:          accID,
			Environment: "demo",
			Balance:     initialBalance,
			Currency:    "USD",
			UpdatedAt:   time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize simulator account: %w", err)
		}
		store.Log("INFO", fmt.Sprintf("Created simulator account with initial balance $%.2f", initialBalance))
	}

	sim := &Simulator{
		store:     store,
		accountID: accID,
		prices:    make(map[string]float64),
	}

	// Start a background loop to oscillate/fluctuate simulator prices
	// and automatically evaluate SL/TP triggers in real time!
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var seed int64 = 0
		for range ticker.C {
			sim.mu.Lock()
			if len(sim.prices) == 0 {
				sim.mu.Unlock()
				continue
			}

			// Generate small oscillations around the price
			for inst, price := range sim.prices {
				seed++
				// deterministic-like fluctuation but pseudo-random
				noise := math.Sin(float64(seed)*0.2) * 0.0001
				instUpper := strings.ToUpper(inst)
				if strings.Contains(instUpper, "BTC") {
					noise = math.Sin(float64(seed)*0.2) * 4.5
				} else if strings.Contains(instUpper, "ETH") {
					noise = math.Sin(float64(seed)*0.2) * 0.4
				}
				sim.prices[inst] = price + noise
			}
			sim.mu.Unlock()

			// Evaluate SL/TP triggers in the background
			_ = sim.EvaluateTriggers()
		}
	}()

	return sim, nil
}

func (s *Simulator) GetPrice(instrument string) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	price, exists := s.prices[instrument]
	return price, exists
}

func (s *Simulator) GetEnvironment() string {
	return "demo"
}

func (s *Simulator) GetBalance() (float64, float64, error) {
	acc, err := s.store.GetAccount(s.accountID)
	if err != nil {
		return 0, 0, err
	}

	positions, err := s.store.GetOpenPositions()
	if err != nil {
		return acc.Balance, acc.Balance, nil
	}

	// Calculate current floating P&L using active ticks
	floatingPL := 0.0
	s.mu.RLock()
	for _, pos := range positions {
		currentPrice, exists := s.prices[pos.Instrument]
		if !exists {
			currentPrice = pos.OpenPrice // Fallback if no ticks yet
		}
		floatingPL += (currentPrice - pos.OpenPrice) * pos.Units
	}
	s.mu.RUnlock()

	return acc.Balance, acc.Balance + floatingPL, nil
}

func (s *Simulator) GetOpenPositions() ([]db.Position, error) {
	return s.store.GetOpenPositions()
}

func (s *Simulator) GetTrades() ([]db.Transaction, error) {
	return s.store.GetTransactions()
}

func (s *Simulator) OpenPosition(instrument string, units float64, currentPrice float64, stopLoss, takeProfit float64) (string, error) {
	acc, err := s.store.GetAccount(s.accountID)
	if err != nil {
		return "", err
	}

	// Simple margin check (leverage 1:30)
	marginRequired := (math.Abs(units) * currentPrice) / 30.0
	if marginRequired > acc.Balance {
		return "", fmt.Errorf("insufficient balance for margin: required $%.2f, balance $%.2f", marginRequired, acc.Balance)
	}

	posID := fmt.Sprintf("sim_pos_%d", time.Now().UnixNano())
	pos := db.Position{
		ID:         posID,
		Instrument: instrument,
		Units:      units,
		OpenPrice:  currentPrice,
		OpenTime:   time.Now(),
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Status:     "OPEN",
	}

	if err := s.store.SavePosition(pos); err != nil {
		return "", err
	}

	txType := "BUY"
	if units < 0 {
		txType = "SELL"
	}

	tx := db.Transaction{
		ID:          fmt.Sprintf("sim_tx_%d", time.Now().UnixNano()),
		Type:        txType,
		Instrument:  instrument,
		Price:       currentPrice,
		Units:       units,
		RealizedPnL: 0.0,
		Timestamp:   time.Now(),
	}

	if err := s.store.SaveTransaction(tx); err != nil {
		return "", err
	}

	s.store.Log("INFO", fmt.Sprintf("Simulator: Open %s position %s. Units: %.2f @ %.5f, SL: %.5f, TP: %.5f", txType, instrument, units, currentPrice, stopLoss, takeProfit))

	return posID, nil
}

func (s *Simulator) ClosePosition(id string, currentPrice float64) error {
	pos, err := s.store.GetPosition(id)
	if err != nil {
		return err
	}

	if pos.Status == "CLOSED" {
		return fmt.Errorf("position already closed")
	}

	acc, err := s.store.GetAccount(s.accountID)
	if err != nil {
		return err
	}

	// Calculate realized P&L
	// P&L = (ClosePrice - OpenPrice) * Units
	pnl := (currentPrice - pos.OpenPrice) * pos.Units

	pos.Status = "CLOSED"
	pos.ClosePrice = &currentPrice
	now := time.Now()
	pos.CloseTime = &now
	pos.RealizedPnL = &pnl

	if err := s.store.SavePosition(pos); err != nil {
		return err
	}

	// Update account balance
	acc.Balance += pnl
	acc.UpdatedAt = time.Now()
	if err := s.store.SaveAccount(acc); err != nil {
		return err
	}

	tx := db.Transaction{
		ID:          fmt.Sprintf("sim_tx_%d", time.Now().UnixNano()),
		Type:        "CLOSE",
		Instrument:  pos.Instrument,
		Price:       currentPrice,
		Units:       -pos.Units, // opposite units
		RealizedPnL: pnl,
		Timestamp:   time.Now(),
	}

	if err := s.store.SaveTransaction(tx); err != nil {
		return err
	}

	s.store.Log("INFO", fmt.Sprintf("Simulator: Closed position %s %s. Close Price: %.5f, Realized PnL: $%.2f. New Balance: $%.2f", pos.ID, pos.Instrument, currentPrice, pnl, acc.Balance))

	return nil
}

func (s *Simulator) UpdatePrices(prices map[string]float64) error {
	s.mu.Lock()
	for inst, price := range prices {
		s.prices[inst] = price
	}
	s.mu.Unlock()

	return s.EvaluateTriggers()
}

func (s *Simulator) EvaluateTriggers() error {
	openPos, err := s.store.GetOpenPositions()
	if err != nil {
		return err
	}

	for _, pos := range openPos {
		s.mu.RLock()
		price, exists := s.prices[pos.Instrument]
		s.mu.RUnlock()
		if !exists {
			continue
		}

		// Check Take Profit / Stop Loss
		triggerClose := false
		triggerPrice := price
		reason := ""

		if pos.Units > 0 { // Long
			if pos.StopLoss > 0 && price <= pos.StopLoss {
				triggerClose = true
				triggerPrice = pos.StopLoss // Execute at stop price
				reason = "STOP LOSS"
			} else if pos.TakeProfit > 0 && price >= pos.TakeProfit {
				triggerClose = true
				triggerPrice = pos.TakeProfit
				reason = "TAKE PROFIT"
			}
		} else if pos.Units < 0 { // Short
			if pos.StopLoss > 0 && price >= pos.StopLoss {
				triggerClose = true
				triggerPrice = pos.StopLoss
				reason = "STOP LOSS"
			} else if pos.TakeProfit > 0 && price <= pos.TakeProfit {
				triggerClose = true
				triggerPrice = pos.TakeProfit
				reason = "TAKE PROFIT"
			}
		}

		if triggerClose {
			s.store.Log("INFO", fmt.Sprintf("Simulator: TP/SL Triggered (%s) on position %s for %s at price %.5f", reason, pos.ID, pos.Instrument, triggerPrice))
			if err := s.ClosePosition(pos.ID, triggerPrice); err != nil {
				s.store.Log("ERROR", fmt.Sprintf("Simulator: failed to execute TP/SL close: %v", err))
			}
		}
	}

	return nil
}

// OandaBroker implements ExecutionEngine on top of live Oanda API
type OandaBroker struct {
	client    *oanda.Client
	store     *db.DB
	accountID string
}

func NewOandaBroker(client *oanda.Client, store *db.DB, accountID string) *OandaBroker {
	return &OandaBroker{
		client:    client,
		store:     store,
		accountID: accountID,
	}
}

func (ob *OandaBroker) GetEnvironment() string {
	return "real"
}

func (ob *OandaBroker) GetBalance() (float64, float64, error) {
	bal, nav, _, err := ob.client.GetAccountSummary()
	if err != nil {
		return 0, 0, err
	}
	// Sync account details to DB
	_ = ob.store.SaveAccount(db.Account{
		ID:          ob.accountID,
		Environment: "real",
		Balance:     bal,
		Currency:    "USD",
		UpdatedAt:   time.Now(),
	})
	return bal, nav, nil
}

func (ob *OandaBroker) GetOpenPositions() ([]db.Position, error) {
	return ob.store.GetOpenPositions()
}

func (ob *OandaBroker) GetTrades() ([]db.Transaction, error) {
	return ob.store.GetTransactions()
}

func (ob *OandaBroker) OpenPosition(instrument string, units float64, currentPrice float64, stopLoss, takeProfit float64) (string, error) {
	resp, err := ob.client.PlaceMarketOrder(instrument, units, stopLoss, takeProfit)
	if err != nil {
		return "", err
	}

	fillPrice, _ := strconv.ParseFloat(resp.OrderFillTransaction.Price, 64)
	if fillPrice == 0 {
		fillPrice = currentPrice
	}

	posID := resp.OrderFillTransaction.ID
	pos := db.Position{
		ID:         posID,
		Instrument: instrument,
		Units:      units,
		OpenPrice:  fillPrice,
		OpenTime:   resp.OrderFillTransaction.Timestamp,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Status:     "OPEN",
	}

	if err := ob.store.SavePosition(pos); err != nil {
		ob.store.Log("ERROR", fmt.Sprintf("Broker: Failed to save position to DB: %v", err))
	}

	txType := "BUY"
	if units < 0 {
		txType = "SELL"
	}

	tx := db.Transaction{
		ID:          resp.OrderFillTransaction.ID,
		Type:        txType,
		Instrument:  instrument,
		Price:       fillPrice,
		Units:       units,
		RealizedPnL: 0,
		Timestamp:   resp.OrderFillTransaction.Timestamp,
	}
	_ = ob.store.SaveTransaction(tx)

	ob.store.Log("INFO", fmt.Sprintf("Broker: Successfully placed MARKET order. ID: %s, Units: %.2f @ %.5f", posID, units, fillPrice))
	return posID, nil
}

func (ob *OandaBroker) ClosePosition(id string, currentPrice float64) error {
	pos, err := ob.store.GetPosition(id)
	if err != nil {
		return err
	}

	if pos.Status == "CLOSED" {
		return fmt.Errorf("position already closed")
	}

	// Execute close on Oanda
	err = ob.client.ClosePosition(pos.Instrument)
	if err != nil {
		return err
	}

	// Fetch fresh balance
	bal, _, _, _ := ob.client.GetAccountSummary()

	// Update local database
	pnl := (currentPrice - pos.OpenPrice) * pos.Units
	pos.Status = "CLOSED"
	pos.ClosePrice = &currentPrice
	now := time.Now()
	pos.CloseTime = &now
	pos.RealizedPnL = &pnl

	_ = ob.store.SavePosition(pos)

	// Sync local account balance
	acc, err := ob.store.GetAccount(ob.accountID)
	if err == nil {
		acc.Balance = bal
		acc.UpdatedAt = time.Now()
		_ = ob.store.SaveAccount(acc)
	}

	tx := db.Transaction{
		ID:          fmt.Sprintf("broker_tx_%d", time.Now().UnixNano()),
		Type:        "CLOSE",
		Instrument:  pos.Instrument,
		Price:       currentPrice,
		Units:       -pos.Units,
		RealizedPnL: pnl,
		Timestamp:   time.Now(),
	}
	_ = ob.store.SaveTransaction(tx)

	ob.store.Log("INFO", fmt.Sprintf("Broker: Closed position %s. Realized PnL: $%.2f", pos.ID, pnl))
	return nil
}

func (ob *OandaBroker) UpdatePrices(prices map[string]float64) error {
	return nil
}

func (ob *OandaBroker) GetPrice(instrument string) (float64, bool) {
	return 0, false
}
