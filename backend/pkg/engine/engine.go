package engine

import (
	"tiq/backend/pkg/db"
)

type ExecutionEngine interface {
	GetBalance() (float64, float64, error) // Returns (balance, equity, error)
	GetOpenPositions() ([]db.Position, error)
	GetTrades() ([]db.Transaction, error)
	OpenPosition(instrument string, units float64, currentPrice float64, stopLoss, takeProfit float64) (string, error)
	ClosePosition(id string, currentPrice float64) error
	UpdatePrices(prices map[string]float64) error // For simulator to evaluate SL/TP triggers
	GetEnvironment() string                        // "demo" or "real"
	GetPrice(instrument string) (float64, bool)
}
