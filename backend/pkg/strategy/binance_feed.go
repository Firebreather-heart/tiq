package strategy

// BinanceFeed: push-based BTC spot feed over Binance's public WebSocket.
//
// Why this exists: the original spot pipeline polled Kraken's REST ticker once
// per second through a regional proxy (Kraken is geo-blocked here), giving the
// strategy a ~1.5-2.5s-stale view of the market — slow enough that Polymarket's
// makers finished repricing before we evaluated (observed live on the 11:41
// July 13 dump: NO went $0.62→$0.96 inside our blind spot). Binance is directly
// reachable from this machine, so a pushed miniTicker stream cuts effective
// staleness to ~0.5-1s AND eliminates the per-second proxy polling (data-use
// drops from hundreds of MB/day to ~15MB/day).
//
// Same-source rule (important): the strategy's signal is spot MINUS strike.
// Binance trades BTC against USDT, which floats a few tens of dollars from
// USD. That offset cancels ONLY if the strike and the live spot come from the
// same feed. While this feed is enabled, spot, candles, and window-open strike
// values must ALL come from Binance — never mix in a Kraken number. The
// wiring in strategy.go enforces this: feed enabled => no Kraken calls at all.
//
// Set BINANCE_FEED=off to revert to the legacy all-Kraken polling path.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tiq/backend/pkg/db"
	"tiq/backend/pkg/oanda"
)

const (
	// data-stream/data-api .binance.vision are Binance's OFFICIAL public
	// market-data mirrors (identical prices/klines to the main hosts). The main
	// hosts (stream.binance.com / api.binance.com) became DNS-blocked from this
	// network on Jul 15 — the .vision mirrors remained reachable. Same source,
	// so the spot/strike same-source rule is unaffected.
	binanceWSURL    = "wss://data-stream.binance.vision:443/ws/btcusdt@miniTicker"
	binanceREST     = "https://data-api.binance.vision"
	binanceSymbol   = "BTCUSDT"
	feedStaleAfter  = 5 * time.Second // miniTicker pushes ~1/s; 5s silent = dead
	maxCandleHist   = 240             // 20 hours of 5m candles, plenty for ATR/EMA(25)
	jumpThresholdUS = 20.0            // $ move between consecutive ticks that forces an immediate strategy tick
)

type BinanceFeed struct {
	store *db.DB

	mu         sync.RWMutex
	lastPrice  float64
	lastUpdate time.Time
	candles    []oanda.Candle // synthetic 5m candles built from the stream (ATR/EMA inputs)

	windowOpenMu sync.Mutex
	windowOpens  map[int64]float64 // startTS -> authoritative kline open (strike anchor)

	// onJump fires (debounced) when price moves >= jumpThresholdUS between
	// consecutive pushes — the "act during the dump, not after" hook.
	onJump     func()
	lastJumpAt time.Time
}

// FeedEnabled reports whether the Binance feed should be used (default on for
// crypto; BINANCE_FEED=off reverts to the legacy Kraken polling path).
func FeedEnabled() bool {
	return !strings.EqualFold(os.Getenv("BINANCE_FEED"), "off")
}

func NewBinanceFeed(store *db.DB, onJump func()) *BinanceFeed {
	return &BinanceFeed{
		store:       store,
		windowOpens: make(map[int64]float64),
		onJump:      onJump,
	}
}

// Start bootstraps candle history over REST and runs the WS loop forever
// (reconnect with backoff). Call once, in a goroutine.
func (f *BinanceFeed) Start() {
	if hist, err := fetchBinanceCandles("BTC_USD", maxCandleHist); err == nil && len(hist) > 0 {
		f.mu.Lock()
		f.candles = hist
		f.lastPrice = hist[len(hist)-1].Close
		f.lastUpdate = time.Now()
		f.mu.Unlock()
		f.store.Log("INFO", fmt.Sprintf("[Binance Feed] Bootstrapped %d 5m candles (last close $%.2f).", len(hist), hist[len(hist)-1].Close))
	} else {
		f.store.Log("WARN", fmt.Sprintf("[Binance Feed] Candle bootstrap failed: %v — will retry via stream.", err))
	}

	backoff := 2 * time.Second
	for {
		if err := f.runWS(); err != nil {
			f.store.Log("WARN", fmt.Sprintf("[Binance Feed] Stream ended: %v — reconnecting in %s.", err, backoff))
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (f *BinanceFeed) runWS() error {
	conn, _, err := websocket.DefaultDialer.Dial(binanceWSURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	f.store.Log("INFO", "[Binance Feed] WebSocket connected (btcusdt@miniTicker, pushed ~1/s).")

	for {
		// miniTicker arrives every ~1s; a long silence means the conn is dead
		// even if TCP hasn't noticed. (gorilla answers server pings for us.)
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		// NOTE: the payload carries both "e" (event type, string) and "E"
		// (event time, ms number). Go's JSON matching is case-insensitive
		// unless an exact tag exists, so EventTime MUST be declared — without
		// it the numeric "E" lands on the string "e" field and every message
		// fails to unmarshal (bit us in the first smoke test).
		var tick struct {
			EventType string `json:"e"`
			EventTime int64  `json:"E"`
			Symbol    string `json:"s"`
			Close     string `json:"c"`
		}
		if json.Unmarshal(msg, &tick) != nil || tick.Symbol != binanceSymbol {
			continue
		}
		price, err := strconv.ParseFloat(tick.Close, 64)
		if err != nil || price <= 0 {
			continue
		}
		f.ingest(price)
	}
}

func (f *BinanceFeed) ingest(price float64) {
	now := time.Now()

	f.mu.Lock()
	prev := f.lastPrice
	f.lastPrice = price
	f.lastUpdate = now

	// Maintain synthetic 5m candles for the indicator stack. The candle OPEN
	// here is "first pushed price after the boundary" (±1s of the true open);
	// good enough for ATR/EMA, but the STRIKE always uses the authoritative
	// REST kline open via WindowOpen instead.
	bucket := now.UTC().Truncate(5 * time.Minute)
	if n := len(f.candles); n > 0 && f.candles[n-1].Time.Equal(bucket) {
		c := &f.candles[n-1]
		c.Close = price
		if price > c.High {
			c.High = price
		}
		if price < c.Low {
			c.Low = price
		}
	} else {
		f.candles = append(f.candles, oanda.Candle{Time: bucket, Open: price, High: price, Low: price, Close: price})
		if len(f.candles) > maxCandleHist {
			f.candles = f.candles[len(f.candles)-maxCandleHist:]
		}
	}
	f.mu.Unlock()

	// Burst detector: a >=$20 jump between consecutive 1s pushes is exactly the
	// fast-move case the 1s poll loop kept missing. Debounced to 1/2s.
	if f.onJump != nil && prev > 0 && (price-prev >= jumpThresholdUS || prev-price >= jumpThresholdUS) {
		f.windowOpenMu.Lock()
		fire := time.Since(f.lastJumpAt) > 2*time.Second
		if fire {
			f.lastJumpAt = time.Now()
		}
		f.windowOpenMu.Unlock()
		if fire {
			f.store.Log("INFO", fmt.Sprintf("[Binance Feed] Spot jump $%.2f -> $%.2f — forcing immediate strategy tick.", prev, price))
			go f.onJump()
		}
	}
}

// Price returns the latest pushed price and whether it is fresh enough to use.
func (f *BinanceFeed) Price() (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastPrice, f.lastPrice > 0 && time.Since(f.lastUpdate) <= feedStaleAfter
}

// Healthy reports whether the stream is delivering.
func (f *BinanceFeed) Healthy() bool {
	_, ok := f.Price()
	return ok
}

// Candles returns up to count most-recent synthetic 5m candles.
func (f *BinanceFeed) Candles(count int) []oanda.Candle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := len(f.candles)
	if n == 0 {
		return nil
	}
	if count > n {
		count = n
	}
	out := make([]oanda.Candle, count)
	copy(out, f.candles[n-count:])
	return out
}

// RefreshREST does a one-shot REST price fetch (same source as the stream) for
// gap-cover while the WS reconnects. Updates the feed state on success.
func (f *BinanceFeed) RefreshREST() (float64, error) {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(binanceREST + "/api/v3/ticker/price?symbol=" + binanceSymbol)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("binance ticker HTTP %d", resp.StatusCode)
	}
	var out struct {
		Price string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	price, err := strconv.ParseFloat(out.Price, 64)
	if err != nil || price <= 0 {
		return 0, fmt.Errorf("bad price %q", out.Price)
	}
	f.ingest(price)
	return price, nil
}

// WindowOpen returns the AUTHORITATIVE open price of the 5m kline starting at
// startTS — the strike anchor. One tiny REST call per contract window, cached.
// (The synthetic stream candle's open can be ±1 tick off the true open; the
// strike is the one number where that matters, so it gets the real kline.)
func (f *BinanceFeed) WindowOpen(startTS int64) (float64, bool) {
	f.windowOpenMu.Lock()
	if v, ok := f.windowOpens[startTS]; ok {
		f.windowOpenMu.Unlock()
		return v, true
	}
	// Bound the cache; windows roll every 5 minutes.
	for ts := range f.windowOpens {
		if ts < startTS-3600 {
			delete(f.windowOpens, ts)
		}
	}
	f.windowOpenMu.Unlock()

	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=5m&startTime=%d&limit=1", binanceREST, binanceSymbol, startTS*1000)
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var raw [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || len(raw) == 0 || len(raw[0]) < 2 {
		return 0, false
	}
	tsMs, ok := raw[0][0].(float64)
	if !ok || int64(tsMs)/1000 != startTS {
		return 0, false // kline for this exact window not published yet
	}
	openStr, _ := raw[0][1].(string)
	open, err := strconv.ParseFloat(openStr, 64)
	if err != nil || open <= 0 {
		return 0, false
	}

	f.windowOpenMu.Lock()
	f.windowOpens[startTS] = open
	f.windowOpenMu.Unlock()
	return open, true
}
