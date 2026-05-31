package strategy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
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

	instUpper := strings.ToUpper(instrument)
	isCrypto := strings.Contains(instUpper, "BTC") || strings.Contains(instUpper, "ETH")

	// Try fetching live crypto candles from CoinGecko first, falling back to Coinbase, Kraken, and finally Binance
	if isCrypto {
		candles, err = fetchCoinGeckoCandles(instrument, count)
		if err != nil {
			r.store.Log("WARN", fmt.Sprintf("Failed to fetch CoinGecko candles for %s: %v. Trying Coinbase backup...", instrument, err))
			candles, err = fetchCoinbaseCandles(instrument, count)
			if err != nil {
				r.store.Log("WARN", fmt.Sprintf("Failed to fetch Coinbase candles for %s: %v. Trying Kraken backup...", instrument, err))
				candles, err = fetchKrakenCandles(instrument, count)
				if err != nil {
					r.store.Log("WARN", fmt.Sprintf("Failed to fetch Kraken candles for %s: %v. Trying Binance backup...", instrument, err))
					candles, err = fetchBinanceCandles(instrument, count)
					if err != nil {
						r.store.Log("WARN", fmt.Sprintf("Failed to fetch Binance candles for %s: %v. Falling back to default provider.", instrument, err))
					}
				}
			}
		}

		if len(candles) > 0 {
			// Initialize or update simulator base price with the real live crypto close price
			r.engine.UpdatePrices(map[string]float64{instrument: candles[len(candles)-1].Close})
		}
	}

	// Fallback to Oanda or dummy generator if live crypto feeds failed, weren't applicable, or returned no data
	if len(candles) == 0 {
		if r.oandaClient == nil {
			// Return dummy candles for mockup/testing if no key is configured
			now := time.Now()
			basePrice := 1.0850
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

// fetchCoinbaseCandles retrieves 24/7 spot crypto market candles from Coinbase's public Exchange REST API
func fetchCoinbaseCandles(symbol string, count int) ([]oanda.Candle, error) {
	// Map instrument (e.g. BTC_USD -> BTC-USD)
	coinbaseSymbol := strings.ReplaceAll(symbol, "_", "-")

	url := fmt.Sprintf("https://api.exchange.coinbase.com/products/%s/candles?granularity=300&limit=%d", coinbaseSymbol, count)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Coinbase Exchange API requires a User-Agent header to prevent 403 Forbidden
	req.Header.Set("User-Agent", "TIQ-AI-Agent/1.0")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coinbase api returned status code %d", resp.StatusCode)
	}

	var rawCandles [][]float64
	if err := json.NewDecoder(resp.Body).Decode(&rawCandles); err != nil {
		return nil, err
	}

	var candles []oanda.Candle
	// Coinbase returns candles in reverse chronological order (latest first), so parse backwards
	for i := len(rawCandles) - 1; i >= 0; i-- {
		item := rawCandles[i]
		if len(item) < 6 {
			continue
		}

		t := time.Unix(int64(item[0]), 0)
		candles = append(candles, oanda.Candle{
			Time:   t,
			Volume: int(item[5]),
			Open:   item[3],
			High:   item[2],
			Low:    item[1],
			Close:  item[4],
		})
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

	url := fmt.Sprintf("https://api.kraken.com/0/public/OHLC?pair=%s&interval=5", pair)

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

