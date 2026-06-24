package engine

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math"
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
	rpcURL              string
	clobURL             string
	yesTokenID          string
	noTokenID           string
	activeMarketAddress string
	wsConn              *websocket.Conn
	mu                  sync.RWMutex
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
			Balance:     5000.00, // Seed with $5k USDC
			Currency:    "USDC",
			UpdatedAt:   time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Polymarket wallet account: %w", err)
		}
		store.Log("INFO", fmt.Sprintf("Web3 Polymarket Wallet initialized. Address: %s, USDC Balance: $5,000.00", walletAddr))
	}

	engine := &PolymarketEngine{
		store:         store,
		walletAddress: walletAddr,
		prices:        make(map[string]float64),
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
			currentPrice, exists := p.prices[pos.Instrument]
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

	return posID, nil
}

func (p *PolymarketEngine) ClosePosition(id string, currentPrice float64) error {
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
	return nil
}

func (p *PolymarketEngine) UpdatePrices(prices map[string]float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range prices {
		p.prices[k] = v
	}
	return nil
}

func (p *PolymarketEngine) GetEnvironment() string {
	return "demo"
}

func (p *PolymarketEngine) GetPrice(instrument string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	price, exists := p.prices[instrument]
	return price, exists
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

		// Exclude further SL/TP checking if token price is not active in feed
		p.mu.RLock()
		yesPrice, exists := p.prices[pos.Instrument]
		p.mu.RUnlock()
		if !exists {
			continue
		}

		// The price of our owned share token
		price := yesPrice
		if pos.Units < 0 {
			price = 1.0 - yesPrice
		}

		// 2. Stop Loss check: if current token price meets or drops below stop-loss trigger price, sell/exit early
		slTrigger := pos.StopLoss
		if slTrigger <= 0 {
			// Fallback to entry price if no stop loss specified
			slTrigger = pos.OpenPrice
		}
		if price <= slTrigger {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Stop Loss triggered (Current Share Price: $%.2f <= Trigger: $%.2f). Closing early.", price, slTrigger))
			_ = p.ClosePosition(pos.ID, price)
			continue
		}

		// 3. Take Profit check: if current token price meets or exceeds target predicted probability, sell/exit early to lock in profits
		if pos.TakeProfit > 0 && price >= pos.TakeProfit {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Take Profit triggered (Current Share Price: $%.2f >= Predicted Target: $%.2f). Locking in profit.", price, pos.TakeProfit))
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

func (p *PolymarketEngine) StartWSListener() {
	for {
		p.store.Log("INFO", "[Polymarket WS] Connecting to CLOB WebSocket wss://ws-subscriptions-clob.polymarket.com/ws/market ...")
		dialer := websocket.DefaultDialer
		conn, _, err := dialer.Dial("wss://ws-subscriptions-clob.polymarket.com/ws/market", nil)
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

		// Send subscription if we already have tokens
		if yesToken != "" && noToken != "" {
			err = p.sendSubscription(conn, yesToken, noToken)
			if err != nil {
				p.store.Log("WARN", fmt.Sprintf("[Polymarket WS] Subscription send failed: %v", err))
			}
		}

		// Read loop
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
	p.mu.Lock()
	oldYes := p.yesTokenID
	oldNo := p.noTokenID
	p.yesTokenID = yesToken
	p.noTokenID = noToken
	p.activeMarketAddress = marketAddr
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

		// Update engine's price feed
		p.mu.Lock()
		p.prices[marketAddr] = yesPrice
		p.mu.Unlock()

		p.store.Log("INFO", fmt.Sprintf("[Polymarket WS] Executed Trade print: %s outcome at $%.3f (Size: %s shares, Side: %s)", 
			outcome, price, sizeStr, side))

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
