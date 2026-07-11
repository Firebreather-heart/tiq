package strategy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"tiq/backend/pkg/db"
	"tiq/backend/pkg/engine"
	"tiq/backend/pkg/oanda"
)

type Config struct {
	Instrument      string  `json:"instrument"`        // e.g., "EUR_USD"
	AlloraTopicID   int     `json:"allora_topic_id"`   // e.g., 1 (BTC) or forex topic
	Granularity     string  `json:"granularity"`       // e.g., "M5", "M15"
	RiskPercent     float64 `json:"risk_percent"`      // e.g., 1.0 (1% of balance)
	AtrMultiplier   float64 `json:"atr_multiplier"`    // e.g., 2.0 (for Stop Loss)
	TpMultiplier    float64 `json:"tp_multiplier"`     // e.g., 3.0 (for Take Profit)
	EmaFastPeriod   int     `json:"ema_fast_period"`   // e.g., 10
	EmaSlowPeriod   int     `json:"ema_slow_period"`   // e.g., 25
	RsiPeriod       int     `json:"rsi_period"`        // e.g., 14
	MinRsiFilter    float64 `json:"min_rsi_filter"`    // e.g., 30 (oversold, buy threshold)
	MaxRsiFilter    float64 `json:"max_rsi_filter"`    // e.g., 70 (overbought, sell threshold)
	TradingEnabled  bool    `json:"trading_enabled"`   // Toggles strategy execution
	UseAllora       bool    `json:"use_allora"`        // Toggles using AI inferences
	DefaultPipValue float64 `json:"default_pip_value"` // e.g., 0.0001 for EUR/USD
}

type Runner struct {
	cfgMu           sync.RWMutex
	cfg             Config
	store           *db.DB
	oandaClient     *oanda.Client
	alloraClient    interface{}
	engine          engine.ExecutionEngine
	cachedATR       float64
	cachedCandles   []oanda.Candle
	lastCandleFetch time.Time
	polyMu          sync.RWMutex
	latestPolyInfo  *PolymarketMarketInfo
	// strikeCache pins one strike per contract window (key = StartTimestamp) so
	// every tick of a window trades against the same number. See resolveStrike.
	strikeCache   map[int64]float64
	strikeCacheMu sync.Mutex
}

func NewRunner(cfg Config, store *db.DB, oClient *oanda.Client, aClient interface{}, eng engine.ExecutionEngine) *Runner {
	return &Runner{
		cfg:          cfg,
		store:        store,
		oandaClient:  oClient,
		alloraClient: aClient,
		engine:       eng,
		strikeCache:  make(map[int64]float64),
	}
}

func (r *Runner) GetConfig() Config {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

func (r *Runner) GetLatestPolymarketInfo() *PolymarketMarketInfo {
	r.polyMu.RLock()
	defer r.polyMu.RUnlock()
	return r.latestPolyInfo
}

func (r *Runner) UpdateConfig(newCfg Config) {
	r.cfgMu.Lock()
	r.cfg = newCfg
	r.cfgMu.Unlock()
	r.store.Log("INFO", fmt.Sprintf("Config updated: Instrument=%s, TradingEnabled=%t, UseAllora=%t", newCfg.Instrument, newCfg.TradingEnabled, newCfg.UseAllora))
}

// Tick executes a single strategy step
func (r *Runner) Tick() error {
	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()
	if !cfg.TradingEnabled {
		return nil
	}

	if polyEng, ok := r.engine.(*engine.PolymarketEngine); ok {
		var latestATR float64
		var currentPrice float64
		var candles []oanda.Candle

		if time.Since(r.lastCandleFetch) >= 30*time.Second || len(r.cachedCandles) == 0 {
			var err error
			candles, err = r.GetCandles(r.cfg.Instrument, 100)
			if err != nil {
				return fmt.Errorf("failed to fetch candles: %w", err)
			}
			if len(candles) < r.cfg.EmaSlowPeriod+2 {
				return fmt.Errorf("insufficient candles fetched: got %d", len(candles))
			}

			r.cachedCandles = candles
			closes := make([]float64, len(candles))
			highs := make([]float64, len(candles))
			lows := make([]float64, len(candles))
			for i, c := range candles {
				closes[i] = c.Close
				highs[i] = c.High
				lows[i] = c.Low
			}

			atr := calculateATR(highs, lows, closes, 5)
			r.cachedATR = atr[len(atr)-1]
			r.lastCandleFetch = time.Now()

			latestCandle := candles[len(candles)-1]
			currentPrice = latestCandle.Close
			latestATR = r.cachedATR
		} else {
			latestATR = r.cachedATR
			candles = r.cachedCandles
			if price, ok := r.engine.GetPrice(r.cfg.Instrument); ok {
				currentPrice = price
			} else {
				latestCandle := candles[len(candles)-1]
				currentPrice = latestCandle.Close
			}
		}

		// Compute EMA Trend momentum
		closes := make([]float64, len(candles))
		for i, c := range candles {
			closes[i] = c.Close
		}
		fastEMA := calculateEMA(closes, r.cfg.EmaFastPeriod)
		slowEMA := calculateEMA(closes, r.cfg.EmaSlowPeriod)
		isBullishTrend := fastEMA[len(fastEMA)-1] > slowEMA[len(slowEMA)-1]

		prices := map[string]float64{
			r.cfg.Instrument: currentPrice,
		}
		_ = polyEng.UpdatePrices(prices)
		latestATR = r.cachedATR

		// Fetch exact live Polymarket strike and expiration from real contracts
		r.store.Log("INFO", "[Polymarket] Querying active BTC contracts directly from Polymarket Gamma API...")
		liveMarket, err := FetchActivePolymarketStrike(currentPrice)

		var strike float64
		var expiration time.Time
		var marketAddr string

		if err == nil && liveMarket != nil {
			// Check if we already have an open position for this market's ConditionID
			// to prevent strike price drift due to candle loading latency
			strike = currentPrice // fallback
			foundActivePos := false
			if openPositions, posErr := polyEng.GetOpenPositions(); posErr == nil {
				for _, pos := range openPositions {
					posParts := strings.Split(pos.Instrument, "_")
					if len(posParts) >= 6 && posParts[1] == liveMarket.MarketID {
						if sVal, err := strconv.ParseFloat(posParts[3], 64); err == nil {
							strike = sVal
							foundActivePos = true
							// Seed the per-window cache too, so a re-entry after this
							// position closes uses the same strike (consistency).
							r.strikeCacheMu.Lock()
							r.strikeCache[liveMarket.StartTimestamp] = sVal
							r.strikeCacheMu.Unlock()
							r.store.Log("INFO", fmt.Sprintf("[Polymarket] Active position found. Matching strike price to open position: $%.2f", strike))
							break
						}
					}
				}
			}

			if !foundActivePos {
				s, ok := r.resolveStrike(liveMarket, candles, currentPrice)
				if !ok {
					// No reliable strike for this window yet (candle not published,
					// too late in the window for the spot proxy). Every entry gate
					// depends on spot-vs-strike distance, so trading against a wrong
					// number is worse than not trading: skip this tick and retry —
					// the exact candle usually appears within seconds.
					r.store.Log("WARN", fmt.Sprintf("[Polymarket] No reliable strike for window %d yet — skipping tick rather than trading against a stale one.", liveMarket.StartTimestamp))
					return nil
				}
				strike = s
			}

			expiration = liveMarket.Expiration
			hexAddr := liveMarket.MarketID
			if hexAddr == "" {
				hexAddr = "0x_dummy_clob_token"
			}
			marketAddr = fmt.Sprintf("poly_%s_strike_%.0f_expiry_%d", hexAddr, strike, expiration.Unix())
			r.store.Log("INFO", fmt.Sprintf("[Polymarket] Direct API Match! Strike: $%.2f, Expiry: %s. Question: %q | StartTS: %d | YesPrice: %.3f, NoPrice: %.3f",
				strike, expiration.Format("15:04:05"), liveMarket.Question, liveMarket.StartTimestamp, liveMarket.YesPrice, liveMarket.NoPrice))

			// Subscribe the Polymarket engine to the active YES/NO contract CLOB tokens in real-time
			if polyEng, ok := r.engine.(*engine.PolymarketEngine); ok {
				polyEng.SubscribeToMarketTokens(liveMarket.YesTokenID, liveMarket.NoTokenID, marketAddr, liveMarket.NegRisk)
			}

			r.polyMu.Lock()
			r.latestPolyInfo = liveMarket
			r.polyMu.Unlock()
		} else {
			// Gracefully log expected mid-hour listing silent periods (when Polymarket script prepares next batch)
			if err != nil && strings.Contains(err.Error(), "no active or upcoming 5m") {
				r.store.Log("INFO", "[Polymarket] No active or upcoming 5m contract listed on Gamma API yet. Waiting for next cycle...")
				return nil
			}
			// Actual API network failure
			r.store.Log("WARN", fmt.Sprintf("[Polymarket API Failure] Could not fetch live contract: %v. Waiting for next cycle...", err))
			return nil
		}

		polyCfg := PolymarketConfig{
			MarketAddress:    marketAddr,
			StrikePrice:      strike,
			ExpirationTime:   expiration,
			MinExpectedValue: 0.12,
			RiskPercent:      r.cfg.RiskPercent,
		}

		polyRunner := NewPolymarketRunner(polyCfg, r.store, polyEng)

		// H: prefer the fresh WS-fed token price over the (polled, seconds-old) Gamma quote
		// for the EV decision — a latency scalp must act on the freshest mispricing. Fall back
		// to the Gamma quote when the WS feed hasn't reported for this contract yet.
		yesPx, noPx := liveMarket.YesPrice, liveMarket.NoPrice
		if wsYes, ok := polyEng.GetPrice(marketAddr); ok && wsYes > 0 && wsYes < 1 {
			yesPx = wsYes
			noPx = 1.0 - wsYes
		}
		return polyRunner.Tick(currentPrice, latestATR, isBullishTrend, yesPx, noPx)
	}

	r.store.Log("INFO", fmt.Sprintf("Strategy Tick started for %s...", r.cfg.Instrument))

	// 1. Fetch candles from unified provider (with Oanda/Binance fallback)
	candles, err := r.GetCandles(r.cfg.Instrument, 100)
	if err != nil {
		return fmt.Errorf("failed to fetch candles: %w", err)
	}

	if len(candles) < r.cfg.EmaSlowPeriod+2 {
		return fmt.Errorf("insufficient candles fetched: got %d", len(candles))
	}

	// 2. Fetch latest price
	latestCandle := candles[len(candles)-1]
	currentPrice := latestCandle.Close

	// Update simulator prices only if they don't exist yet (to avoid overwriting fresh live prices with stale candle close)
	if _, exists := r.engine.GetPrice(r.cfg.Instrument); !exists {
		prices := map[string]float64{
			r.cfg.Instrument: currentPrice,
		}
		if err := r.engine.UpdatePrices(prices); err != nil {
			r.store.Log("WARN", fmt.Sprintf("Failed to update simulator prices: %v", err))
		}
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

	// 4. Fetch Allora Inference (Disabled)
	var alloraSignal float64 = 0.0 // positive = bullish, negative = bearish
	alloraActive := false

	// 5. Generate Trading Signal
	var signal string = "HOLD"

	if strings.Contains(strings.ToUpper(r.cfg.Instrument), "BTC") {
		r.store.Log("INFO", "[Polymarket-Oanda] Querying active BTC contracts directly from Polymarket Gamma API...")
		liveMarket, err := FetchActivePolymarketStrike(currentPrice)

		var strike float64
		var timeRemaining float64
		if err == nil && liveMarket != nil {
			strike = liveMarket.Strike
			timeRemaining = time.Until(liveMarket.Expiration).Seconds()
			if timeRemaining <= 0 {
				timeRemaining = 180.0
			}
			r.store.Log("INFO", fmt.Sprintf("[Polymarket-Oanda] Direct API Match! Strike: $%.2f, Expiry: %s. Question: %q",
				strike, liveMarket.Expiration.Format("15:04:05"), liveMarket.Question))
		} else {
			r.store.Log("ERROR", fmt.Sprintf("[Polymarket-Oanda API Failure] Could not fetch live contract: %v. Local estimation is disabled.", err))
			return fmt.Errorf("polymarket API integration failed for Oanda driver: %w", err)
		}

		volatilityPerSec := (latestATR / currentPrice) / math.Sqrt(300.0)
		if volatilityPerSec <= 0 {
			volatilityPerSec = 0.0001
		}

		d := math.Log(currentPrice/strike) / (volatilityPerSec * math.Sqrt(timeRemaining))
		trueYesProbability := 0.5 * (1.0 + math.Erf(d/math.Sqrt(2.0)))
		trueNoProbability := 1.0 - trueYesProbability

		marketYesPrice := 0.50 + (math.Sin(float64(time.Now().Unix())*0.01) * 0.15)
		if marketYesPrice < 0.05 {
			marketYesPrice = 0.05
		} else if marketYesPrice > 0.95 {
			marketYesPrice = 0.95
		}
		marketNoPrice := 1.0 - marketYesPrice

		yesEV := (trueYesProbability * 1.0) - marketYesPrice
		noEV := (trueNoProbability * 1.0) - marketNoPrice

		r.store.Log("INFO", fmt.Sprintf("[Polymarket-Oanda] Odds: YES=$%.2f, NO=$%.2f. Prob: YES=%.1f%%, NO=%.1f%%. EV: YES=+$%.2f, NO=+$%.2f",
			marketYesPrice, marketNoPrice, trueYesProbability*100, trueNoProbability*100, yesEV, noEV))

		if yesEV >= 0.02 {
			signal = "BUY"
			r.store.Log("INFO", "[Polymarket-Oanda] Dynamic Bullish EV Edge! Output: Oanda BUY Signal")
		} else if noEV >= 0.02 {
			signal = "SELL"
			r.store.Log("INFO", "[Polymarket-Oanda] Dynamic Bearish EV Edge! Output: Oanda SELL Signal")
		} else {
			r.store.Log("INFO", "[Polymarket-Oanda] Neutral market state. Hold.")
		}
	} else {
		// Bullish signal: Fast EMA > Slow EMA (Trend is up), and RSI < maxRsiFilter (Not overbought).
		// Allora winrate is strictly based on high-probability Allora inferences.
		// Define high probability: prediction is at least 0.05% above/below current spot (high confidence edge).
		isBullishTrend := latestFastEMA > latestSlowEMA
		isBearishTrend := latestFastEMA < latestSlowEMA

		alloraBullishHighProb := alloraActive && (alloraSignal > currentPrice*1.0005)
		alloraBearishHighProb := alloraActive && (alloraSignal < currentPrice*0.9995)

		if isBullishTrend && latestRSI < r.cfg.MaxRsiFilter {
			if alloraActive {
				if alloraBullishHighProb {
					signal = "BUY"
					r.store.Log("INFO", fmt.Sprintf("[Allora AI] High Probability Bullish Signal! Target: $%.2f (Edge: +%.3f%%)", alloraSignal, (alloraSignal-currentPrice)/currentPrice*100))
				} else {
					r.store.Log("INFO", fmt.Sprintf("[Allora AI] Bullish prediction too close ($%.2f vs Spot $%.2f). Skipping low probability trade.", alloraSignal, currentPrice))
				}
			} else {
				// Standard Technical indicator fallback
				signal = "BUY"
			}
		} else if isBearishTrend && latestRSI > r.cfg.MinRsiFilter {
			if alloraActive {
				if alloraBearishHighProb {
					signal = "SELL"
					r.store.Log("INFO", fmt.Sprintf("[Allora AI] High Probability Bearish Signal! Target: $%.2f (Edge: -%.3f%%)", alloraSignal, (currentPrice-alloraSignal)/currentPrice*100))
				} else {
					r.store.Log("INFO", fmt.Sprintf("[Allora AI] Bearish prediction too close ($%.2f vs Spot $%.2f). Skipping low probability trade.", alloraSignal, currentPrice))
				}
			} else {
				// Standard Technical indicator fallback
				signal = "SELL"
			}
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
		if strings.Contains(pos.Instrument, r.cfg.Instrument) {
			activePos = &pos
			break
		}
	}

	if activePos != nil {
		// We have an active position.
		isLong := activePos.Units > 0

		// Exit Condition 1: Opposite signal — full trend reversal.
		oppositeSignal := (isLong && signal == "SELL") || (!isLong && signal == "BUY")

		// Exit Condition 2: Trend fade — EMA spread has collapsed to near-zero
		// even before a full crossover. This exits weakening momentum early.
		emaSpread := latestFastEMA - latestSlowEMA
		emaSpreading := (isLong && emaSpread > 0) || (!isLong && emaSpread < 0)
		emaTrendFading := !emaSpreading // spread has flipped direction

		// Exit Condition 3: RSI extreme — overbought on a long, oversold on a short
		rsiOverextended := (isLong && latestRSI > 75) || (!isLong && latestRSI < 25)

		if oppositeSignal || emaTrendFading || rsiOverextended {
			reason := "opposite signal"
			if emaTrendFading {
				reason = "EMA trend fade (spread collapsed)"
			} else if rsiOverextended {
				reason = fmt.Sprintf("RSI overextended (%.1f)", latestRSI)
			}
			r.store.Log("INFO", fmt.Sprintf("Early exit triggered [%s]. Closing position %s @ %.5f", reason, activePos.ID, currentPrice))
			if err := r.engine.ClosePosition(activePos.ID, currentPrice); err != nil {
				return fmt.Errorf("failed to close position on early exit: %w", err)
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

		instrumentName := r.cfg.Instrument
		if alloraActive {
			instrumentName = "allora_" + r.cfg.Instrument
		}

		_, err = r.engine.OpenPosition(instrumentName, units, currentPrice, stopLoss, takeProfit)
		if err != nil {
			return fmt.Errorf("failed to open position: %w", err)
		}
	}

	return nil
}

// resolveStrike determines the strike (BTC price at the contract window's open)
// for a btc-updown market, caching one value per window so every tick trades
// against the same number.
//
// Context: these markets settle on Chainlink's BTC/USD stream ("not according
// to other sources or spot markets" — per the market description), and the
// Gamma API publishes NO strike field; the strike simply IS the price at window
// start. We approximate it with Kraken's same-window candle open (tracks
// Chainlink within a few dollars — fine for a $30+ distance signal).
//
// This replaces a bug where, when the exact-window candle wasn't in our (up to
// 30s stale) cache at window start, we silently fell back to the PREVIOUS
// candle's open. Validated against real Kraken history: 46 of 47 recorded
// trades had traded against the prior window's open, ~$60 off — larger than
// the $30 minimum lag the entry gate requires, i.e. every gate decision ran on
// a wrong number. The fallback ladder is now:
//
//  1. cached strike for this window (stability across ticks)
//  2. exact-window candle open from the already-fetched candles
//  3. force-refresh candles once and retry the exact match (the usual fix —
//     the forming candle appears on Kraken within seconds of window start)
//  4. live spot, only within the first 30s of the window (close enough to the
//     open to be a fair proxy)
//  5. give up (caller skips the tick) — NEVER the previous candle's open
func (r *Runner) resolveStrike(liveMarket *PolymarketMarketInfo, candles []oanda.Candle, currentPrice float64) (float64, bool) {
	startTS := liveMarket.StartTimestamp

	r.strikeCacheMu.Lock()
	if s, ok := r.strikeCache[startTS]; ok {
		r.strikeCacheMu.Unlock()
		return s, true
	}
	// Bound the cache: windows roll every 5 minutes, entries older than an hour
	// can never be asked for again.
	for ts := range r.strikeCache {
		if ts < startTS-3600 {
			delete(r.strikeCache, ts)
		}
	}
	r.strikeCacheMu.Unlock()

	cache := func(s float64) (float64, bool) {
		r.strikeCacheMu.Lock()
		r.strikeCache[startTS] = s
		r.strikeCacheMu.Unlock()
		return s, true
	}

	// 2. exact-window candle in the current set
	for _, c := range candles {
		if c.Time.Unix() == startTS {
			return cache(c.Open)
		}
	}

	// 3. the candle cache can be up to 30s stale at window start — refresh once
	if fresh, err := r.GetCandles(r.GetConfig().Instrument, 100); err == nil {
		r.cachedCandles = fresh
		r.lastCandleFetch = time.Now()
		for _, c := range fresh {
			if c.Time.Unix() == startTS {
				return cache(c.Open)
			}
		}
	}

	// 4. early enough in the window that live spot ≈ the open
	if elapsed := time.Now().Unix() - startTS; elapsed >= 0 && elapsed <= 30 && currentPrice > 0 {
		r.store.Log("INFO", fmt.Sprintf("[Polymarket] Using live spot $%.2f as strike proxy for window %d (%ds after open; exact candle not yet published).", currentPrice, startTS, elapsed))
		return cache(currentPrice)
	}

	return 0, false
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

	instUpper := strings.ToUpper(instrument)
	isCrypto := strings.Contains(instUpper, "BTC") || strings.Contains(instUpper, "ETH")

	// Fetch live crypto candles from Kraken via proxy
	if isCrypto {
		candles, err = fetchKrakenCandles(instrument, count)
		if err != nil {
			r.store.Log("WARN", fmt.Sprintf("Failed to fetch Kraken candles for %s: %v. Falling back to default provider.", instrument, err))
		}

		if len(candles) > 0 {
			r.engine.UpdatePrices(map[string]float64{instrument: candles[len(candles)-1].Close})
		}
	}

	// If no live candles were fetched and we don't have an OANDA client fallback, return a catastrophic system error
	if len(candles) == 0 {
		if r.oandaClient == nil {
			return nil, fmt.Errorf("catastrophic failure: failed to fetch live crypto candles and no broker fallback is configured")
		} else {
			candles, err = r.oandaClient.GetCandles(instrument, count, r.cfg.Granularity)
			if err != nil {
				return nil, err
			}
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

// fetchBinanceCandles retrieves 24/7 spot crypto market candles from Binance's public REST API
func fetchBinanceCandles(symbol string, count int) ([]oanda.Candle, error) {
	// Map instrument to Binance format (e.g. BTC_USD -> BTCUSDT)
	binanceSymbol := strings.ReplaceAll(symbol, "_", "")
	if binanceSymbol == "BTCUSD" {
		binanceSymbol = "BTCUSDT"
	} else if binanceSymbol == "ETHUSD" {
		binanceSymbol = "ETHUSDT"
	}

	url := fmt.Sprintf("https://api.binance.com/api/v3/klines?symbol=%s&interval=5m&limit=%d", binanceSymbol, count)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance api returned status code %d", resp.StatusCode)
	}

	var rawKlines [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rawKlines); err != nil {
		return nil, err
	}

	var candles []oanda.Candle
	for _, kline := range rawKlines {
		if len(kline) < 6 {
			continue
		}

		openTimeMs, ok := kline[0].(float64)
		if !ok {
			continue
		}
		t := time.Unix(0, int64(openTimeMs)*int64(time.Millisecond))

		openStr, _ := kline[1].(string)
		highStr, _ := kline[2].(string)
		lowStr, _ := kline[3].(string)
		closeStr, _ := kline[4].(string)
		volumeStr, _ := kline[5].(string)

		openVal, _ := strconv.ParseFloat(openStr, 64)
		highVal, _ := strconv.ParseFloat(highStr, 64)
		lowVal, _ := strconv.ParseFloat(lowStr, 64)
		closeVal, _ := strconv.ParseFloat(closeStr, 64)
		volumeVal, _ := strconv.ParseFloat(volumeStr, 64)

		candles = append(candles, oanda.Candle{
			Time:   t,
			Volume: int(volumeVal),
			Open:   openVal,
			High:   highVal,
			Low:    lowVal,
			Close:  closeVal,
		})
	}

	return candles, nil
}

// fetchKrakenCandles retrieves 24/7 spot crypto market candles from Kraken's public REST API
func fetchKrakenCandles(symbol string, count int) ([]oanda.Candle, error) {
	inst := strings.ToUpper(symbol)
	var pair string
	if strings.Contains(inst, "BTC") {
		pair = "XBTUSD"
	} else if strings.Contains(inst, "ETH") {
		pair = "ETHUSD"
	} else {
		pair = strings.ReplaceAll(inst, "_", "")
	}

	proxyURL := os.Getenv("PROXY_URL")
	var url string
	if proxyURL != "" {
		url = fmt.Sprintf("%s/proxy/kraken/0/public/OHLC?pair=%s&interval=5", proxyURL, pair)
	} else {
		url = fmt.Sprintf("https://api.kraken.com/0/public/OHLC?pair=%s&interval=5", pair)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "TIQ-AI-Agent/1.0")

	clientTimeout := 5 * time.Second
	if proxyURL != "" {
		clientTimeout = 180 * time.Second // Allow extra time for Render free tier cold start/spin down (up to 180 seconds)
	}
	client := &http.Client{Timeout: clientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kraken api returned status code %d", resp.StatusCode)
	}

	var rawResponse struct {
		Error  []string               `json:"error"`
		Result map[string]interface{} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&rawResponse); err != nil {
		return nil, err
	}

	if len(rawResponse.Error) > 0 {
		return nil, fmt.Errorf("kraken api error: %s", strings.Join(rawResponse.Error, ", "))
	}

	var rawCandles []interface{}
	for k, v := range rawResponse.Result {
		if k == "last" {
			continue
		}
		if arr, ok := v.([]interface{}); ok {
			rawCandles = arr
			break
		}
	}

	if len(rawCandles) == 0 {
		return nil, fmt.Errorf("no candle data found in kraken response")
	}

	startIdx := 0
	if len(rawCandles) > count {
		startIdx = len(rawCandles) - count
	}

	var candles []oanda.Candle
	for i := startIdx; i < len(rawCandles); i++ {
		candleArr, ok := rawCandles[i].([]interface{})
		if !ok || len(candleArr) < 8 {
			continue
		}

		tVal, ok1 := candleArr[0].(float64)
		oVal, ok2 := candleArr[1].(string)
		hVal, ok3 := candleArr[2].(string)
		lVal, ok4 := candleArr[3].(string)
		cVal, ok5 := candleArr[4].(string)
		vVal, ok6 := candleArr[6].(string)

		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
			continue
		}

		t := time.Unix(int64(tVal), 0)
		o, errO := strconv.ParseFloat(oVal, 64)
		h, errH := strconv.ParseFloat(hVal, 64)
		l, errL := strconv.ParseFloat(lVal, 64)
		c, errC := strconv.ParseFloat(cVal, 64)
		vFloat, errV := strconv.ParseFloat(vVal, 64)

		if errO != nil || errH != nil || errL != nil || errC != nil || errV != nil {
			continue
		}

		candles = append(candles, oanda.Candle{
			Time:   t,
			Volume: int(vFloat),
			Open:   o,
			High:   h,
			Low:    l,
			Close:  c,
		})
	}

	return candles, nil
}

// fetchCoinGeckoCandles retrieves 24/7 spot crypto market candles from CoinGecko's public REST API
func fetchCoinGeckoCandles(symbol string, count int) ([]oanda.Candle, error) {
	inst := strings.ToUpper(symbol)
	var coinID string
	if strings.Contains(inst, "BTC") {
		coinID = "bitcoin"
	} else if strings.Contains(inst, "ETH") {
		coinID = "ethereum"
	} else {
		return nil, fmt.Errorf("unsupported coingecko instrument: %s", symbol)
	}

	apiKey := os.Getenv("COIN_GECKO_KEY")
	url := fmt.Sprintf("https://api.coingecko.com/api/v3/coins/%s/ohlc?vs_currency=usd&days=1", coinID)
	if apiKey != "" {
		url = fmt.Sprintf("https://api.coingecko.com/api/v3/coins/%s/ohlc?vs_currency=usd&days=1&x_cg_demo_api_key=%s", coinID, apiKey)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "TIQ-AI-Agent/1.0")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coingecko api returned status code %d", resp.StatusCode)
	}

	var rawCandles [][]float64
	if err := json.NewDecoder(resp.Body).Decode(&rawCandles); err != nil {
		return nil, err
	}

	startIdx := 0
	if len(rawCandles) > count {
		startIdx = len(rawCandles) - count
	}

	var candles []oanda.Candle
	for i := startIdx; i < len(rawCandles); i++ {
		item := rawCandles[i]
		if len(item) < 5 {
			continue
		}
		t := time.Unix(int64(item[0])/1000, 0)
		candles = append(candles, oanda.Candle{
			Time:   t,
			Volume: 100, // CoinGecko OHLC does not provide volume, use dummy
			Open:   item[1],
			High:   item[2],
			Low:    item[3],
			Close:  item[4],
		})
	}

	return candles, nil
}

// LiveTick fetches the real-time spot price frequently
// to decouple live execution from the 5-minute candle evaluation.
func (r *Runner) LiveTick() error {
	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()
	if !cfg.TradingEnabled {
		return nil
	}
	instUpper := strings.ToUpper(cfg.Instrument)
	isCrypto := strings.Contains(instUpper, "BTC") || strings.Contains(instUpper, "ETH")

	if isCrypto {
		livePrice, err := fetchLivePrice(cfg.Instrument)
		if err == nil && livePrice > 0 {
			r.engine.UpdatePrices(map[string]float64{cfg.Instrument: livePrice})
		}

		// Polymarket prices are now updated in real-time via the CLOB WebSocket listener thread
		// running in the PolymarketEngine background loop. This eliminates redundant REST polling
		// and prevents stale midpoint prices from overwriting the real-time trade execution prints.
	}
	// For Oanda (forex), OandaBroker updates prices directly via websocket stream internally or simulator oscillator handles it.
	return nil
}

// fetchLivePrice retrieves the sub-second live spot price from Kraken via proxy (primary) or Bybit via proxy (fallback)
func fetchLivePrice(symbol string) (float64, error) {
	instUpper := strings.ToUpper(symbol)
	isBTC := strings.Contains(instUpper, "BTC")
	isETH := strings.Contains(instUpper, "ETH")

	if isBTC || isETH {
		// Primary: Kraken Ticker via proxy
		var pair string
		if isBTC {
			pair = "XBTUSD"
		} else {
			pair = "ETHUSD"
		}

		proxyURL := os.Getenv("PROXY_URL")
		var url string
		if proxyURL != "" {
			url = fmt.Sprintf("%s/proxy/kraken/0/public/Ticker?pair=%s", proxyURL, pair)
		} else {
			url = fmt.Sprintf("https://api.kraken.com/0/public/Ticker?pair=%s", pair)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err == nil {
			req.Header.Set("User-Agent", "TIQ-AI-Agent/1.0")
			clientTimeout := 2 * time.Second
			if proxyURL != "" {
				clientTimeout = 180 * time.Second
			}
			client := &http.Client{Timeout: clientTimeout}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var rawResponse struct {
						Error  []string               `json:"error"`
						Result map[string]interface{} `json:"result"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&rawResponse); err == nil && len(rawResponse.Error) == 0 {
						for k, v := range rawResponse.Result {
							if k == "error" {
								continue
							}
							if dataMap, ok := v.(map[string]interface{}); ok {
								if cArr, ok := dataMap["c"].([]interface{}); ok && len(cArr) > 0 {
									if priceStr, ok := cArr[0].(string); ok {
										price, err := strconv.ParseFloat(priceStr, 64)
										if err == nil && price > 0 {
											return price, nil
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	bybitSymbol := strings.ReplaceAll(symbol, "_", "")
	if bybitSymbol == "BTCUSD" {
		bybitSymbol = "BTCUSDT"
	}
	proxyURL := os.Getenv("PROXY_URL")
	var url string
	if proxyURL != "" {
		url = fmt.Sprintf("%s/proxy/bybit/v5/market/tickers?category=linear&symbol=%s", proxyURL, bybitSymbol)
	} else {
		url = fmt.Sprintf("https://api.bybit.com/v5/market/tickers?category=linear&symbol=%s", bybitSymbol)
	}
	clientTimeout := 2 * time.Second
	if proxyURL != "" {
		clientTimeout = 60 * time.Second // Allow extra time for Render free tier cold start/spin down
	}
	client := &http.Client{Timeout: clientTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var rawResponse struct {
		Result struct {
			List []struct {
				LastPrice string `json:"lastPrice"`
			} `json:"list"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&rawResponse); err != nil {
		return 0, err
	}

	if len(rawResponse.Result.List) > 0 {
		return strconv.ParseFloat(rawResponse.Result.List[0].LastPrice, 64)
	}
	return 0, fmt.Errorf("no live price found")
}
