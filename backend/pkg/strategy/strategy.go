package strategy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"tiq/backend/pkg/allora"
	"tiq/backend/pkg/db"
	"tiq/backend/pkg/engine"
	"tiq/backend/pkg/oanda"
)

type Config struct {
	Instrument       string  `json:"instrument"`         // e.g., "EUR_USD"
	AlloraTopicID    int     `json:"allora_topic_id"`     // e.g., 1 (BTC) or forex topic
	Granularity      string  `json:"granularity"`         // e.g., "M5", "M15"
	RiskPercent      float64 `json:"risk_percent"`        // e.g., 1.0 (1% of balance)
	AtrMultiplier    float64 `json:"atr_multiplier"`      // e.g., 2.0 (for Stop Loss)
	TpMultiplier     float64 `json:"tp_multiplier"`       // e.g., 3.0 (for Take Profit)
	EmaFastPeriod    int     `json:"ema_fast_period"`     // e.g., 10
	EmaSlowPeriod    int     `json:"ema_slow_period"`     // e.g., 25
	RsiPeriod        int     `json:"rsi_period"`          // e.g., 14
	MinRsiFilter     float64 `json:"min_rsi_filter"`      // e.g., 30 (oversold, buy threshold)
	MaxRsiFilter     float64 `json:"max_rsi_filter"`      // e.g., 70 (overbought, sell threshold)
	TradingEnabled   bool    `json:"trading_enabled"`     // Toggles strategy execution
	UseAllora        bool    `json:"use_allora"`          // Toggles using AI inferences
	DefaultPipValue  float64 `json:"default_pip_value"`   // e.g., 0.0001 for EUR/USD
}

type Runner struct {
	cfg          Config
	store        *db.DB
	oandaClient  *oanda.Client
	alloraClient *allora.Client
	engine       engine.ExecutionEngine
}

func NewRunner(cfg Config, store *db.DB, oClient *oanda.Client, aClient *allora.Client, eng engine.ExecutionEngine) *Runner {
	return &Runner{
		cfg:          cfg,
		store:        store,
		oandaClient:  oClient,
		alloraClient: aClient,
		engine:       eng,
	}
}

func (r *Runner) GetConfig() Config {
	return r.cfg
}

func (r *Runner) UpdateConfig(newCfg Config) {
	r.cfg = newCfg
	r.store.Log("INFO", fmt.Sprintf("Config updated: Instrument=%s, TradingEnabled=%t, UseAllora=%t", r.cfg.Instrument, r.cfg.TradingEnabled, r.cfg.UseAllora))
}

// Tick executes a single strategy step
func (r *Runner) Tick() error {
	if !r.cfg.TradingEnabled {
		return nil
	}

	r.store.Log("INFO", fmt.Sprintf("Strategy Tick started for %s...", r.cfg.Instrument))

	// 1. Fetch candles from Oanda
	candles, err := r.oandaClient.GetCandles(r.cfg.Instrument, 100, r.cfg.Granularity)
	if err != nil {
		return fmt.Errorf("failed to fetch candles: %w", err)
	}

	if len(candles) < r.cfg.EmaSlowPeriod+2 {
		return fmt.Errorf("insufficient candles fetched: got %d", len(candles))
	}

	// 2. Fetch latest price
	latestCandle := candles[len(candles)-1]
	currentPrice := latestCandle.Close

	// Update simulator prices
	prices := map[string]float64{
		r.cfg.Instrument: currentPrice,
	}
	if err := r.engine.UpdatePrices(prices); err != nil {
		r.store.Log("WARN", fmt.Sprintf("Failed to update simulator prices: %v", err))
	}

	// 3. Compute Technical Indicators
	closes := make([]float64, len(candles))
	highs := make([]float64, len(candles))
	lows := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
		highs[i] = c.High
		lows[i] = c.Low
	}

	fastEMA := calculateEMA(closes, r.cfg.EmaFastPeriod)
	slowEMA := calculateEMA(closes, r.cfg.EmaSlowPeriod)
	rsi := calculateRSI(closes, r.cfg.RsiPeriod)
	atr := calculateATR(highs, lows, closes, 14)

	latestFastEMA := fastEMA[len(fastEMA)-1]
	latestSlowEMA := slowEMA[len(slowEMA)-1]
	latestRSI := rsi[len(rsi)-1]
	latestATR := atr[len(atr)-1]

	r.store.Log("INFO", fmt.Sprintf("Indicators: Price=%.5f, FastEMA=%.5f, SlowEMA=%.5f, RSI=%.2f, ATR=%.5f",
		currentPrice, latestFastEMA, latestSlowEMA, latestRSI, latestATR))

	// 4. Fetch Allora Inference
	var alloraSignal float64 = 0.0 // positive = bullish, negative = bearish
	var blockHeight int64 = 0
	alloraActive := r.cfg.UseAllora

	if r.cfg.UseAllora {
		inf, err := r.alloraClient.GetLatestInference(r.cfg.AlloraTopicID)
		if err != nil {
			alloraActive = false
			r.store.Log("WARN", fmt.Sprintf("Failed to fetch Allora inference: %v. Proceeding with technical indicators only.", err))
		} else {
			alloraSignal = inf.ParsedValue
			blockHeight = inf.BlockHeight

			// Save to local cache
			_ = r.store.SaveAlloraInference(db.AlloraInference{
				TopicID:       r.cfg.AlloraTopicID,
				BlockHeight:   inf.BlockHeight,
				CombinedValue: inf.CombinedValue,
				ParsedValue:   inf.ParsedValue,
				Timestamp:     time.Now(),
			})
			r.store.Log("INFO", fmt.Sprintf("Allora AI Inference: Topic=%d, Block=%d, Value=%.5f", r.cfg.AlloraTopicID, blockHeight, alloraSignal))
		}
	}

	// 5. Generate Trading Signal
	// Bullish signal: Fast EMA > Slow EMA (Trend is up), and RSI < maxRsiFilter (Not overbought).
	// If UseAllora is true, we also check if Allora AI predicted price is greater than current price.
	isBullishTrend := latestFastEMA > latestSlowEMA
	isBearishTrend := latestFastEMA < latestSlowEMA

	var signal string = "HOLD"
	if isBullishTrend && latestRSI < r.cfg.MaxRsiFilter {
		if !alloraActive || alloraSignal > currentPrice {
			signal = "BUY"
		}
	} else if isBearishTrend && latestRSI > r.cfg.MinRsiFilter {
		if !alloraActive || alloraSignal < currentPrice {
			signal = "SELL"
		}
	}

	r.store.Log("INFO", fmt.Sprintf("Decision Signal: %s", signal))

	// 6. Manage Active Positions
	openPosList, err := r.engine.GetOpenPositions()
	if err != nil {
		return fmt.Errorf("failed to fetch open positions: %w", err)
	}

	// Clean up any open positions for inactive instruments
	for _, pos := range openPosList {
		if pos.Instrument != r.cfg.Instrument {
			r.store.Log("INFO", fmt.Sprintf("Closing inactive instrument position: ID=%s, Instrument=%s", pos.ID, pos.Instrument))
			closePrice := pos.OpenPrice
			otherCandles, err := r.oandaClient.GetCandles(pos.Instrument, 1, r.cfg.Granularity)
			if err == nil && len(otherCandles) > 0 {
				closePrice = otherCandles[len(otherCandles)-1].Close
			}
			if err := r.engine.ClosePosition(pos.ID, closePrice); err != nil {
				r.store.Log("ERROR", fmt.Sprintf("Failed to close inactive instrument position %s: %v", pos.ID, err))
			}
		}
	}

	// Re-fetch open positions after cleanup to ensure we only have active ones
	openPosList, err = r.engine.GetOpenPositions()
	if err != nil {
		return fmt.Errorf("failed to fetch open positions after cleanup: %w", err)
	}

	// For simplicity, we manage one position per instrument at a time
	var activePos *db.Position
	for _, pos := range openPosList {
		if pos.Instrument == r.cfg.Instrument {
			activePos = &pos
			break
		}
	}

	if activePos != nil {
		// We have an active position.
		// If the new signal is opposite, close the position
		isLong := activePos.Units > 0
		if (isLong && signal == "SELL") || (!isLong && signal == "BUY") {
			r.store.Log("INFO", fmt.Sprintf("Opposite signal received. Closing active position %s.", activePos.ID))
			if err := r.engine.ClosePosition(activePos.ID, currentPrice); err != nil {
				return fmt.Errorf("failed to close position: %w", err)
			}
			activePos = nil // Closed
		}
	}

	// If no active position, check if we should open one
	if activePos == nil && (signal == "BUY" || signal == "SELL") {
		// Calculate position size based on balance and risk
		bal, _, err := r.engine.GetBalance()
		if err != nil {
			return fmt.Errorf("failed to fetch balance for risk sizing: %w", err)
		}

		riskCash := bal * (r.cfg.RiskPercent / 100.0)

		// Set Stop Loss and Take Profit distance based on ATR
		if latestATR == 0 {
			latestATR = r.cfg.DefaultPipValue * 30.0 // Fallback to 30 pips
		}
		slDistance := latestATR * r.cfg.AtrMultiplier
		tpDistance := latestATR * r.cfg.TpMultiplier

		// Units size = Risk Cash / Stop Loss Distance
		units := riskCash / slDistance

		var stopLoss, takeProfit float64
		if signal == "BUY" {
			stopLoss = currentPrice - slDistance
			takeProfit = currentPrice + tpDistance
		} else { // SELL
			units = -units // negative units represent Short position
			stopLoss = currentPrice + slDistance
			takeProfit = currentPrice - tpDistance
		}

		r.store.Log("INFO", fmt.Sprintf("Execution: Placing %s. Risking $%.2f. Units: %.2f, SL: %.5f, TP: %.5f",
			signal, riskCash, units, stopLoss, takeProfit))

		_, err = r.engine.OpenPosition(r.cfg.Instrument, units, currentPrice, stopLoss, takeProfit)
		if err != nil {
			return fmt.Errorf("failed to open position: %w", err)
		}
	}

	return nil
}

// Indicator helper functions (Standard Go implementation)

func calculateEMA(prices []float64, period int) []float64 {
	ema := make([]float64, len(prices))
	if len(prices) < period {
		return ema
	}

	// Initialize first EMA with simple average
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += prices[i]
	}
	ema[period-1] = sum / float64(period)

	multiplier := 2.0 / (float64(period) + 1.0)
	for i := period; i < len(prices); i++ {
		ema[i] = (prices[i] * multiplier) + (ema[i-1] * (1.0 - multiplier))
	}

	return ema
}

func calculateRSI(prices []float64, period int) []float64 {
	rsi := make([]float64, len(prices))
	if len(prices) < period+1 {
		return rsi
	}

	gains := make([]float64, len(prices))
	losses := make([]float64, len(prices))

	for i := 1; i < len(prices); i++ {
		diff := prices[i] - prices[i-1]
		if diff > 0 {
			gains[i] = diff
			losses[i] = 0
		} else {
			gains[i] = 0
			losses[i] = -diff
		}
	}

	// Calculate initial average gain and loss
	avgGain := 0.0
	avgLoss := 0.0
	for i := 1; i <= period; i++ {
		avgGain += gains[i]
		avgLoss += losses[i]
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	if avgLoss == 0 {
		rsi[period] = 100
	} else {
		rs := avgGain / avgLoss
		rsi[period] = 100.0 - (100.0 / (1.0 + rs))
	}

	for i := period + 1; i < len(prices); i++ {
		avgGain = ((avgGain * float64(period-1)) + gains[i]) / float64(period)
		avgLoss = ((avgLoss * float64(period-1)) + losses[i]) / float64(period)

		if avgLoss == 0 {
			rsi[i] = 100
		} else {
			rs := avgGain / avgLoss
			rsi[i] = 100.0 - (100.0 / (1.0 + rs))
		}
	}

	return rsi
}

func calculateATR(highs, lows, closes []float64, period int) []float64 {
	atr := make([]float64, len(closes))
	if len(closes) < period+1 {
		return atr
	}

	tr := make([]float64, len(closes))
	tr[0] = highs[0] - lows[0]

	for i := 1; i < len(closes); i++ {
		hl := highs[i] - lows[i]
		hc := math.Abs(highs[i] - closes[i-1])
		lc := math.Abs(lows[i] - closes[i-1])
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}

	// First ATR is the simple average of True Ranges
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += tr[i]
	}
	atr[period-1] = sum / float64(period)

	for i := period; i < len(closes); i++ {
		atr[i] = ((atr[i-1] * float64(period-1)) + tr[i]) / float64(period)
	}

	return atr
}

func (r *Runner) GetCandles(instrument string, count int) ([]oanda.Candle, error) {
	var candles []oanda.Candle
	var err error

	if r.oandaClient == nil {
		// Return dummy candles for mockup/testing if no key is configured
		now := time.Now()
		basePrice := 1.0850
		instUpper := strings.ToUpper(instrument)
		if strings.Contains(instUpper, "BTC") {
			basePrice = 73500.00
		} else if strings.Contains(instUpper, "ETH") {
			basePrice = 3500.00
		} else if strings.Contains(instUpper, "GBP") {
			basePrice = 1.2700
		}

		for i := 0; i < count; i++ {
			t := now.Add(time.Duration(-count+i) * 5 * time.Minute)
			var change float64
			if basePrice > 1000 {
				change = (math.Sin(float64(i)*0.1) * 100.0) + (math.Cos(float64(i)*0.05) * 50.0)
			} else {
				change = (math.Sin(float64(i)*0.1) * 0.001) + (math.Cos(float64(i)*0.05) * 0.0005)
			}
			closeP := basePrice + change
			var openP, highP, lowP float64
			if basePrice > 1000 {
				openP = closeP - (math.Sin(float64(i)) * 40.0)
				highP = math.Max(openP, closeP) + 30.0
				lowP = math.Min(openP, closeP) - 30.0
			} else {
				openP = closeP - (math.Sin(float64(i)) * 0.0004)
				highP = math.Max(openP, closeP) + 0.0003
				lowP = math.Min(openP, closeP) - 0.0003
			}
			candles = append(candles, oanda.Candle{
				Time:   t,
				Volume: 100 + (i % 50),
				Open:   openP,
				High:   highP,
				Low:    lowP,
				Close:  closeP,
			})
		}
	} else {
		candles, err = r.oandaClient.GetCandles(instrument, count, r.cfg.Granularity)
		if err != nil {
			return nil, err
		}
	}

	// Update the latest candle using the simulator's active live price if it exists
	if price, exists := r.engine.GetPrice(instrument); exists && len(candles) > 0 {
		lastIdx := len(candles) - 1
		candles[lastIdx].Close = price
		if price > candles[lastIdx].High {
			candles[lastIdx].High = price
		}
		if price < candles[lastIdx].Low {
			candles[lastIdx].Low = price
		}
	}

	return candles, nil
}

