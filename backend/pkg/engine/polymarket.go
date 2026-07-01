package engine

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tiq/backend/pkg/db"
)

type PolymarketEngine struct {
	store               *db.DB
	walletAddress       string
	privateKey          *ecdsa.PrivateKey
	prices              map[string]float64
	slPrices            map[string]float64   // Separate price feed for SL checking, only updated by trades >= $20 USDC
	slPriceTimes        map[string]time.Time // Last update time of each slPrices entry (for staleness fallback)
	slStates            map[string]time.Time // Track first breached timestamp for each position ID
	rpcURL              string
	clobURL             string
	yesTokenID          string
	noTokenID           string
	activeMarketAddress string
	wsConn              *websocket.Conn
	structConn          *websocket.Conn // position-scoped Struct WS; nil when flat
	mu                  sync.RWMutex
	accountMu           sync.Mutex // Serializes wallet balance + position open/close (prevents double-close/double-refund races)
	structMu            sync.Mutex // Protects structConn lifecycle (connect on OpenPosition, close on ClosePosition)
}

func NewPolymarketEngine(store *db.DB, pkHex string, rpcURL string) (*PolymarketEngine, error) {
	// For simulation / development, we seed the wallet address from mock hex
	walletAddr := "0x71C7656EC7ab88b098defB751B7401B5f6d1476B"
	
	// Create default Web3 wallet balance if not exists (USDC on Polygon)
	accID := "polymarket_wallet_" + walletAddr
	_, err := store.GetAccount(accID)
	if err != nil {
		err = store.SaveAccount(db.Account{
			ID:          accID,
			Environment: "demo",
			Balance:     100.00,
			Currency:    "USDC",
			UpdatedAt:   time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Polymarket wallet account: %w", err)
		}
		store.Log("INFO", fmt.Sprintf("Web3 Polymarket Wallet initialized. Address: %s, USDC Balance: $100.00", walletAddr))
	}

	engine := &PolymarketEngine{
		store:         store,
		walletAddress: walletAddr,
		prices:        make(map[string]float64),
		slPrices:      make(map[string]float64),
		slPriceTimes:  make(map[string]time.Time),
		slStates:      make(map[string]time.Time),
		rpcURL:        rpcURL,
		clobURL:       "https://clob.polymarket.com",
	}

	// Start the real-time WebSocket connection to Polymarket CLOB
	go engine.StartWSListener()

	// Dynamic trigger evaluator loop for Polymarket positions
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			_ = engine.EvaluatePositionTriggers()
		}
	}()

	return engine, nil
}

func (p *PolymarketEngine) GetBalance() (float64, float64, error) {
	accID := "polymarket_wallet_" + p.walletAddress
	acc, err := p.store.GetAccount(accID)
	if err != nil {
		return 0, 0, err
	}

	// Calculate equity: wallet balance + nominal value of open YES/NO contract shares
	openPositions, err := p.store.GetOpenPositions()
	if err != nil {
		return acc.Balance, acc.Balance, nil
	}

	equity := acc.Balance
	for _, pos := range openPositions {
		if stringsHasPrefix(pos.ID, "poly_") {
			p.mu.RLock()
			currentPrice, exists := p.prices[priceKey(pos.Instrument)]
			p.mu.RUnlock()
			if !exists {
				currentPrice = pos.OpenPrice
			}
			
			// Value of shares = number of shares * current share price
			equity += math.Abs(pos.Units) * currentPrice
		}
	}

	return acc.Balance, equity, nil
}

func (p *PolymarketEngine) GetOpenPositions() ([]db.Position, error) {
	allPos, err := p.store.GetOpenPositions()
	if err != nil {
		return nil, err
	}

	var polyPos []db.Position
	for _, pos := range allPos {
		if stringsHasPrefix(pos.ID, "poly_") {
			polyPos = append(polyPos, pos)
		}
	}
	return polyPos, nil
}

func (p *PolymarketEngine) GetTrades() ([]db.Transaction, error) {
	allTx, err := p.store.GetTransactions()
	if err != nil {
		return nil, err
	}

	var polyTx []db.Transaction
	for _, tx := range allTx {
		if stringsHasPrefix(tx.ID, "tx_poly_") || stringsHasPrefix(tx.ID, "tx_close_") {
			polyTx = append(polyTx, tx)
		}
	}
	return polyTx, nil
}

// OpenPosition acts as the buy executor for Polymarket YES/NO tokens
// units > 0 = Buy YES tokens, units < 0 = Buy NO tokens
func (p *PolymarketEngine) OpenPosition(market string, units float64, currentPrice float64, stopLoss, takeProfit float64) (string, error) {
	// Read tokens before accountMu to keep lock ordering consistent (mu always before accountMu).
	p.mu.RLock()
	yToken := p.yesTokenID
	nToken := p.noTokenID
	p.mu.RUnlock()

	// Serialize all wallet-balance mutations so concurrent opens/closes can't corrupt the balance.
	p.accountMu.Lock()
	defer p.accountMu.Unlock()

	accID := "polymarket_wallet_" + p.walletAddress
	acc, err := p.store.GetAccount(accID)
	if err != nil {
		return "", err
	}

	cost := math.Abs(units) * currentPrice
	if acc.Balance < cost {
		return "", fmt.Errorf("insufficient USDC balance in Web3 wallet: have %.2f, need %.2f", acc.Balance, cost)
	}

	// 1. Simulate EIP-712 Order signature
	p.store.Log("INFO", fmt.Sprintf("[Web3 CLOB] Signed EIP-712 buy order for %s outcome. Wallet: %s", market, p.walletAddress))

	// Deduct USDC from wallet balance
	acc.Balance -= cost
	acc.UpdatedAt = time.Now()
	_ = p.store.SaveAccount(acc)

	// Save position to local DB
	posID := fmt.Sprintf("poly_%d", time.Now().UnixNano())
	pos := db.Position{
		ID:          posID,
		Instrument:  market,
		Units:       units,
		OpenPrice:   currentPrice,
		OpenTime:    time.Now(),
		StopLoss:    stopLoss,
		TakeProfit:  takeProfit,
		Status:      "OPEN",
	}
	err = p.store.SavePosition(pos)
	if err != nil {
		return "", err
	}

	// Record transaction
	txType := "BUY_YES"
	if units < 0 {
		txType = "BUY_NO"
	}
	_ = p.store.SaveTransaction(db.Transaction{
		ID:          "tx_" + posID,
		Type:        txType,
		Instrument:  market,
		Price:       currentPrice,
		Units:       units,
		RealizedPnL: 0,
		Timestamp:   time.Now(),
	})

	p.store.Log("INFO", fmt.Sprintf("[Polymarket] Executed transaction. Bought %.2f shares of %s outcome at $%.2f USDC/share", math.Abs(units), market, currentPrice))

	// Connect Struct WS for reliable TP/SL price monitoring during this position.
	// CLOB WS remains active as the background feed; Struct overrides prices while connected.
	if os.Getenv("STRUCT_API_KEY") != "" && yToken != "" && nToken != "" {
		go p.startStructPositionFeed(yToken, nToken)
	}

	return posID, nil
}

func (p *PolymarketEngine) ClosePosition(id string, currentPrice float64) error {
	// Serialize with other open/close calls. Holding accountMu while we re-read the position
	// status closes the check-then-act gap that allowed two goroutines (1s ticker, per-trade
	// WS handler, manual API close) to both refund the same position.
	p.accountMu.Lock()
	defer p.accountMu.Unlock()

	pos, err := p.store.GetPosition(id)
	if err != nil {
		return err
	}
	if pos.Status == "CLOSED" {
		return nil
	}

	// Calculate realized profit/loss
	// Payout is $1.00 USDC per share if won, but if sold back to order book early, we receive currentPrice
	pnl := math.Abs(pos.Units) * (currentPrice - pos.OpenPrice)

	// Refund balance + returns to Web3 wallet
	accID := "polymarket_wallet_" + p.walletAddress
	acc, err := p.store.GetAccount(accID)
	if err == nil {
		payoutAmount := math.Abs(pos.Units) * currentPrice
		acc.Balance += payoutAmount
		acc.UpdatedAt = time.Now()
		_ = p.store.SaveAccount(acc)
	}

	// Update position status
	now := time.Now()
	pos.Status = "CLOSED"
	pos.ClosePrice = &currentPrice
	pos.CloseTime = &now
	pos.RealizedPnL = &pnl
	_ = p.store.SavePosition(pos)

	// Record transaction
	_ = p.store.SaveTransaction(db.Transaction{
		ID:          "tx_close_" + pos.ID,
		Type:        "CLOSE",
		Instrument:  pos.Instrument,
		Price:       currentPrice,
		Units:       pos.Units,
		RealizedPnL: pnl,
		Timestamp:   time.Now(),
	})

	p.store.Log("INFO", fmt.Sprintf("[Polymarket] Sold %.2f shares of %s early at $%.2f USDC/share. Realized PnL: $%.2f USDC", math.Abs(pos.Units), pos.Instrument, currentPrice, pnl))
	
	// Clean up Stop Loss state
	p.mu.Lock()
	delete(p.slStates, id)
	p.mu.Unlock()

	// Disconnect the position-scoped Struct feed — no longer needed when flat.
	p.structMu.Lock()
	if p.structConn != nil {
		p.structConn.Close()
		p.structConn = nil
	}
	p.structMu.Unlock()

	return nil
}

func (p *PolymarketEngine) UpdatePrices(prices map[string]float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range prices {
		p.prices[priceKey(k)] = v
	}
	return nil
}

func (p *PolymarketEngine) GetEnvironment() string {
	return "demo"
}

func (p *PolymarketEngine) GetPrice(instrument string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	price, exists := p.prices[priceKey(instrument)]
	return price, exists
}

// CheckOrderBookLiquidity queries the Struct REST API for resting ask-side depth on the
// token we're about to buy, and returns true only if there is at least requiredUsdc of it.
//
// Fail policy: when a STRUCT_API_KEY is present we FAIL-CLOSED (return false) on any
// missing/thin/unreadable data — better to skip an entry than to buy into a thin book and
// eat slippage. When no key is configured (sim/dev), we FAIL-OPEN with a warning so the
// bot remains usable without a Struct subscription.
func (p *PolymarketEngine) CheckOrderBookLiquidity(isYes bool, requiredUsdc float64) (bool, error) {
	p.mu.RLock()
	yesToken := p.yesTokenID
	noToken := p.noTokenID
	p.mu.RUnlock()

	tokenID := yesToken
	if !isYes {
		tokenID = noToken
	}

	apiKey := os.Getenv("STRUCT_API_KEY")
	if apiKey == "" {
		// No data source configured — fail-open so sim/dev without Struct still trades.
		p.store.Log("WARN", "[Polymarket Engine] STRUCT_API_KEY not set — liquidity depth UNVERIFIED, allowing entry (fail-open). Set the key to enforce depth checks.")
		return true, nil
	}

	if tokenID == "" {
		p.store.Log("WARN", "[Polymarket Engine] Token ID is empty — cannot verify liquidity. Skipping entry (fail-closed).")
		return false, nil
	}

	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("https://api.struct.to/v1/polymarket/order-book?position_id=%s", tokenID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Failed to build liquidity request: %v. Skipping entry (fail-closed).", err))
		return false, nil
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Struct order-book call failed: %v. Skipping entry (fail-closed).", err))
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Struct order-book returned status %d. Skipping entry (fail-closed).", resp.StatusCode))
		return false, nil
	}

	// Struct nests the book under "data"; we use its pre-computed ask-side USD liquidity.
	// Pointers distinguish a real 0 from a null (empty book).
	var ob struct {
		Success bool `json:"success"`
		Data    struct {
			BestAsk         *float64 `json:"best_ask"`
			AskLiquidityUsd *float64 `json:"ask_liquidity_usd"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ob); err != nil {
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Failed to decode order book JSON: %v. Skipping entry (fail-closed).", err))
		return false, nil
	}

	if !ob.Success || ob.Data.AskLiquidityUsd == nil {
		p.store.Log("INFO", "[Polymarket Engine] No ask-side liquidity reported (empty book). Skipping entry (fail-closed).")
		return false, nil
	}

	askUsdc := *ob.Data.AskLiquidityUsd
	bestAsk := 0.0
	if ob.Data.BestAsk != nil {
		bestAsk = *ob.Data.BestAsk
	}

	p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Best Ask: $%.3f | Ask-side Depth: $%.2f USDC (Required: $%.2f USDC)",
		bestAsk, askUsdc, requiredUsdc))

	if askUsdc < requiredUsdc {
		p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Insufficient ask depth: $%.2f USDC < $%.2f USDC. Skipping trade to prevent slippage.", askUsdc, requiredUsdc))
		return false, nil
	}

	return true, nil
}

// EvaluatePositionTriggers dynamically resolves SL/TP/Expiry triggers for open shares
func (p *PolymarketEngine) EvaluatePositionTriggers() error {
	openPos, err := p.GetOpenPositions()
	if err != nil {
		return err
	}

	for _, pos := range openPos {
		// Parse contract information from instrument: poly_0x123abc_strike_69120_expiry_1717320000
		strike := 70500.0
		expiryUnix := pos.OpenTime.Add(5 * time.Minute).Unix() // Default fallback

		parts := strings.Split(pos.Instrument, "_")
		if len(parts) >= 6 {
			if sVal, err := strconv.ParseFloat(parts[3], 64); err == nil {
				strike = sVal
			}
			if eVal, err := strconv.ParseInt(parts[5], 10, 64); err == nil {
				expiryUnix = eVal
			}
		}

		// 0. Scalp flatten: never ride a scalp into settlement (binary $0/$1 gap risk).
		// Force-close 90s before expiry at the live token price.
		if expiryUnix-time.Now().Unix() <= 90 {
			p.mu.RLock()
			yesPx, hasPx := p.prices[priceKey(pos.Instrument)]
			p.mu.RUnlock()
			if hasPx {
				closePx := yesPx
				if pos.Units < 0 {
					closePx = 1.0 - yesPx
				}
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Scalp flatten: %ds to expiry (<=90s). Closing %s at $%.3f to avoid settlement risk.", expiryUnix-time.Now().Unix(), pos.ID, closePx))
				p.mu.Lock()
				delete(p.slStates, pos.ID)
				p.mu.Unlock()
				_ = p.ClosePosition(pos.ID, closePx)
				continue
			}
		}

		// 1. Expiration check: check if expiration time is reached
		if time.Now().Unix() >= expiryUnix {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Live Contract Expiration Reached for %s. Resolving...", pos.ID))
			
			// Get current spot price of BTC
			spotPrice, hasSpot := p.GetPrice("BTC_USD")
			if !hasSpot {
				spotPrice = strike
			}

			isLong := pos.Units > 0
			resolutionPrice := 0.0
			
			if isLong {
				if spotPrice >= strike {
					resolutionPrice = 1.0 // YES won!
					p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Contract YES won! Spot $%.2f >= Strike $%.2f. Settle at $1.00 Payout.", spotPrice, strike))
				} else {
					resolutionPrice = 0.0 // YES lost
					p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Contract YES lost! Spot $%.2f < Strike $%.2f. Settle at $0.00.", spotPrice, strike))
				}
			} else {
				if spotPrice < strike {
					resolutionPrice = 1.0 // NO won!
					p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Contract NO won! Spot $%.2f < Strike $%.2f. Settle at $1.00 Payout.", spotPrice, strike))
				} else {
					resolutionPrice = 0.0 // NO lost
					p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Contract NO lost! Spot $%.2f >= Strike $%.2f. Settle at $0.00.", spotPrice, strike))
				}
			}

			_ = p.ClosePosition(pos.ID, resolutionPrice)
			continue
		}

		// Fetch price for general tracking and Take Profit
		key := priceKey(pos.Instrument)
		p.mu.RLock()
		yesPrice, exists := p.prices[key]
		// Fetch price for Stop Loss (robust SL price feed) plus its freshness
		slYesPrice, slExists := p.slPrices[key]
		slUpdatedAt, slHasTime := p.slPriceTimes[key]
		p.mu.RUnlock()

		if !exists {
			continue
		}
		// Use the robust SL price only if present AND fresh; otherwise fall back to the live
		// feed so a stale large-print price can't suppress (or wrongly trigger) the stop.
		slStale := !slHasTime || time.Since(slUpdatedAt) > slPriceStaleAfter
		if !slExists || slStale {
			if slExists && slStale {
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Robust SL price for %s is stale (%.0fs old > %.0fs). Falling back to live feed for stop check.",
					pos.ID, time.Since(slUpdatedAt).Seconds(), slPriceStaleAfter.Seconds()))
			}
			slYesPrice = yesPrice // Fallback to standard price if no robust SL price yet, or it went stale
		}

		// The price of our owned share token for TP
		price := yesPrice
		if pos.Units < 0 {
			price = 1.0 - yesPrice
		}

		// The price of our owned share token for SL checking
		slPrice := slYesPrice
		if pos.Units < 0 {
			slPrice = 1.0 - slYesPrice
		}

		// 2. Stop Loss check: if robust share price meets or drops below stop-loss trigger price, start confirmation timer
		slTrigger := pos.StopLoss
		if slTrigger <= 0 {
			slTrigger = pos.OpenPrice
		}

		if slPrice <= slTrigger {
			p.mu.Lock()
			breachTime, breached := p.slStates[pos.ID]
			if !breached {
				breachTime = time.Now()
				p.slStates[pos.ID] = breachTime
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Stop Loss threshold breached (Robust Price: $%.2f <= Trigger: $%.2f). Starting 1-second validation timer...", slPrice, slTrigger))
			}
			p.mu.Unlock()

			if time.Since(breachTime) >= 1*time.Second {
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Stop Loss confirmed after 1 second (Robust Price: $%.2f <= Trigger: $%.2f). Closing early.", slPrice, slTrigger))
				p.mu.Lock()
				delete(p.slStates, pos.ID)
				p.mu.Unlock()
				_ = p.ClosePosition(pos.ID, slPrice)
				continue
			}
		} else {
			// Reset confirmation timer if price recovers above SL trigger
			p.mu.Lock()
			if _, breached := p.slStates[pos.ID]; breached {
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Price recovered above Stop Loss (Robust Price: $%.2f > Trigger: $%.2f). Resetting confirmation timer.", slPrice, slTrigger))
				delete(p.slStates, pos.ID)
			}
			p.mu.Unlock()
		}

		// 3. Take Profit check: if current token price meets or exceeds target predicted probability, sell/exit early to lock in profits
		if pos.TakeProfit > 0 && price >= pos.TakeProfit {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Take Profit triggered (Current Share Price: $%.2f >= Predicted Target: $%.2f). Locking in profit.", price, pos.TakeProfit))
			p.mu.Lock()
			delete(p.slStates, pos.ID) // Clear any SL states
			p.mu.Unlock()
			_ = p.ClosePosition(pos.ID, price)
			continue
		}
	}

	return nil
}

// Utility helper
func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// slPriceStaleAfter is how long a robust ("$20+ trade") SL price stays trusted before we
// fall back to the live feed. Prevents a stale large-print price from gating the stop loss.
const slPriceStaleAfter = 10 * time.Second

// priceKey normalizes a Polymarket instrument ("poly_<condID>_strike_..._expiry_...") down to
// its stable condition-ID hex ("poly_<condID>"), so a strike value that drifts between ticks
// can't desync the price feeds (WS writes vs position reads). Non-poly instruments (e.g. spot
// "BTC_USD") are returned unchanged.
func priceKey(instrument string) string {
	if strings.HasPrefix(instrument, "poly_") {
		parts := strings.Split(instrument, "_")
		if len(parts) >= 2 && parts[1] != "" {
			return "poly_" + parts[1]
		}
	}
	return instrument
}

func (p *PolymarketEngine) StartWSListener() {
	for {
		// Primary feed: raw Polymarket CLOB WS (free, direct source, always-on).
		// Struct WS is only activated per-position via startStructPositionFeed.
		wsURL := "wss://ws-subscriptions-clob.polymarket.com/ws/market"
		p.store.Log("INFO", "[Polymarket WS] Connecting to raw CLOB WebSocket (primary feed)...")

		dialer := websocket.DefaultDialer
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			p.store.Log("WARN", fmt.Sprintf("[Polymarket WS] Dial failed: %v. Retrying in 3 seconds...", err))
			time.Sleep(3 * time.Second)
			continue
		}

		p.store.Log("INFO", "[Polymarket WS] Connected successfully.")
		p.mu.Lock()
		p.wsConn = conn
		yesToken := p.yesTokenID
		noToken := p.noTokenID
		p.mu.Unlock()

		if yesToken != "" && noToken != "" {
			if err = p.sendSubscription(conn, yesToken, noToken); err != nil {
				p.store.Log("WARN", fmt.Sprintf("[Polymarket WS] Subscription send failed: %v", err))
			}
		}

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				p.store.Log("WARN", fmt.Sprintf("[Polymarket WS] Read failed: %v. Reconnecting...", err))
				conn.Close()
				break
			}
			p.handleWSMessage(msg)
		}

		time.Sleep(2 * time.Second)
	}
}

func (p *PolymarketEngine) SubscribeToMarketTokens(yesToken, noToken, marketAddr string) {
	activeKey := priceKey(marketAddr)

	p.mu.Lock()
	oldYes := p.yesTokenID
	oldNo := p.noTokenID
	p.yesTokenID = yesToken
	p.noTokenID = noToken
	p.activeMarketAddress = marketAddr

	// Bound the price maps: drop stale poly_ keys from prior contracts (positions never
	// survive past their own contract thanks to the 90s flatten), keeping only the active
	// contract and any non-poly keys (e.g. spot "BTC_USD").
	for k := range p.prices {
		if strings.HasPrefix(k, "poly_") && k != activeKey {
			delete(p.prices, k)
			delete(p.slPrices, k)
			delete(p.slPriceTimes, k)
		}
	}

	conn := p.wsConn
	p.mu.Unlock()

	// If tokens changed and we have a connection, close the connection
	// to trigger an immediate reconnect and clean subscription to the new tokens
	if conn != nil && (yesToken != oldYes || noToken != oldNo) {
		p.store.Log("INFO", fmt.Sprintf("[Polymarket WS] Subscribed market changed. Re-establishing socket for YES: %s, NO: %s...", yesToken, noToken))
		conn.Close()
	}
}

func (p *PolymarketEngine) sendSubscription(conn *websocket.Conn, yesToken, noToken string) error {
	payload := map[string]interface{}{
		"type":                   "market",
		"assets_ids":             []string{yesToken, noToken},
		"custom_feature_enabled": true,
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, bytes)
}

func (p *PolymarketEngine) sendStructSubscription(conn *websocket.Conn, yesToken, noToken string) error {
	// 1. Join room polymarket_trades
	joinPayload := map[string]interface{}{
		"type": "join_room",
		"payload": map[string]interface{}{
			"room_id": "polymarket_trades",
		},
	}
	joinBytes, err := json.Marshal(joinPayload)
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, joinBytes); err != nil {
		return err
	}

	// 2. Subscribe to specific outcome tokens
	subPayload := map[string]interface{}{
		"type": "room_message",
		"payload": map[string]interface{}{
			"room_id": "polymarket_trades",
			"message": map[string]interface{}{
				"action":       "subscribe",
				"position_ids": []string{yesToken, noToken},
			},
		},
	}
	subBytes, err := json.Marshal(subPayload)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, subBytes)
}

func (p *PolymarketEngine) handleWSMessage(msg []byte) {
	var raw interface{}
	if err := json.Unmarshal(msg, &raw); err != nil {
		return
	}

	p.mu.Lock()
	yesToken := p.yesTokenID
	noToken := p.noTokenID
	marketAddr := p.activeMarketAddress
	p.mu.Unlock()

	if yesToken == "" || noToken == "" || marketAddr == "" {
		return
	}

	processEvent := func(ev map[string]interface{}) {
		eventType, _ := ev["event_type"].(string)
		if eventType != "last_trade_price" {
			return
		}

		assetID, _ := ev["asset_id"].(string)
		if assetID != yesToken && assetID != noToken {
			return
		}

		priceStr, _ := ev["price"].(string)
		sizeStr, _ := ev["size"].(string)
		side, _ := ev["side"].(string)

		price, err := strconv.ParseFloat(priceStr, 64)
		if err != nil {
			return
		}

		yesPrice := price
		outcome := "YES"
		if assetID == noToken {
			yesPrice = 1.0 - price
			outcome = "NO"
		}

		// Calculate size in USD for Polymarket raw CLOB WS
		sizeVal := 0.0
		if sizeStr != "" {
			if val, err := strconv.ParseFloat(sizeStr, 64); err == nil {
				sizeVal = val
			}
		}
		usdAmount := sizeVal * price

		key := priceKey(marketAddr)
		p.mu.Lock()
		p.prices[key] = yesPrice
		// Only update robust SL price feed if trade is >= $20 USDC
		if usdAmount >= 20.0 {
			p.slPrices[key] = yesPrice
			p.slPriceTimes[key] = time.Now()
		}
		p.mu.Unlock()

		p.store.Log("INFO", fmt.Sprintf("[Polymarket WS] Executed Trade print: %s outcome at $%.3f (Size: %s shares / $%.2f USDC, Side: %s)", 
			outcome, price, sizeStr, usdAmount, side))

		// Immediately evaluate positions
		_ = p.EvaluatePositionTriggers()
	}

	switch val := raw.(type) {
	case []interface{}:
		for _, item := range val {
			if ev, ok := item.(map[string]interface{}); ok {
				processEvent(ev)
			}
		}
	case map[string]interface{}:
		processEvent(val)
	}
}

// startStructPositionFeed opens a position-scoped Struct WS connection that stays live for exactly
// the duration of one open position. It provides reliable real-time prices for TP/SL monitoring,
// compensating for occasional CLOB WS lag or drops that can delay exits mid-trade.
// The goroutine exits cleanly when ClosePosition closes p.structConn.
func (p *PolymarketEngine) startStructPositionFeed(yesToken, noToken string) {
	apiKey := os.Getenv("STRUCT_API_KEY")
	if apiKey == "" {
		return
	}

	wsURL := fmt.Sprintf("wss://api.struct.to/ws?api-key=%s", apiKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		p.store.Log("WARN", fmt.Sprintf("[Struct Position Feed] Connect failed: %v. CLOB feed remains sole monitor.", err))
		return
	}

	// Register; close any stale connection from a prior position that wasn't cleaned up.
	p.structMu.Lock()
	if p.structConn != nil {
		p.structConn.Close()
	}
	p.structConn = conn
	p.structMu.Unlock()

	p.store.Log("INFO", "[Struct Position Feed] Connected. Monitoring position for reliable TP/SL exits.")

	if err := p.sendStructSubscription(conn, yesToken, noToken); err != nil {
		p.store.Log("WARN", fmt.Sprintf("[Struct Position Feed] Subscription failed: %v. Falling back to CLOB feed.", err))
		conn.Close()
		p.structMu.Lock()
		if p.structConn == conn {
			p.structConn = nil
		}
		p.structMu.Unlock()
		return
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// Normal path: ClosePosition closed the conn; or a network drop.
			p.structMu.Lock()
			if p.structConn == conn {
				p.structConn = nil
			}
			p.structMu.Unlock()
			p.store.Log("INFO", "[Struct Position Feed] Disconnected. CLOB feed resumes sole monitoring.")
			return
		}
		p.handleStructWSMessage(msg, yesToken, noToken)
	}
}

func (p *PolymarketEngine) handleStructWSMessage(msg []byte, yesToken, noToken string) {
	var wsMsg struct {
		Type   string `json:"type"`
		RoomID string `json:"room_id"`
		Data   struct {
			TradeType   string  `json:"trade_type"`
			PositionID  string  `json:"position_id"`
			Price       float64 `json:"price"`
			UsdAmount   float64 `json:"usd_amount"`
			SharesCount float64 `json:"shares_amount"`
			Side        string  `json:"side"`
		} `json:"data"`
	}

	if err := json.Unmarshal(msg, &wsMsg); err != nil {
		return
	}

	if wsMsg.Type != "trade_stream_update" || wsMsg.RoomID != "polymarket_trades" {
		return
	}

	trade := wsMsg.Data
	if trade.TradeType != "OrderFilled" {
		return
	}

	if trade.PositionID != yesToken && trade.PositionID != noToken {
		return
	}

	p.mu.Lock()
	marketAddr := p.activeMarketAddress
	p.mu.Unlock()

	if marketAddr == "" {
		return
	}

	price := trade.Price
	yesPrice := price
	outcome := "YES"
	if trade.PositionID == noToken {
		yesPrice = 1.0 - price
		outcome = "NO"
	}

	key := priceKey(marketAddr)
	p.mu.Lock()
	p.prices[key] = yesPrice
	// Only update robust SL price feed if trade is >= $20 USDC
	if trade.UsdAmount >= 20.0 {
		p.slPrices[key] = yesPrice
		p.slPriceTimes[key] = time.Now()
	}
	p.mu.Unlock()

	p.store.Log("INFO", fmt.Sprintf("[Struct WS] Executed Trade print: %s outcome at $%.3f (Size: $%.2f USDC, Side: %s)", 
		outcome, price, trade.UsdAmount, trade.Side))

	// Immediately evaluate positions
	_ = p.EvaluatePositionTriggers()
}
