package engine

import (
	"crypto/ecdsa"
	crand "crypto/rand"
	"encoding/hex"
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

// structKeyManager rotates through Struct API keys on consecutive failures.
// After 3 consecutive errors on the active key it promotes the next key in the list.
type structKeyManager struct {
	keys      []string
	activeIdx int
	errCount  int
	mu        sync.Mutex
}

func newStructKeyManager() *structKeyManager {
	var keys []string
	if k := os.Getenv("STRUCT_API_KEY"); k != "" {
		keys = append(keys, k)
	}
	if k := os.Getenv("STRUCT_API_KEY_2"); k != "" {
		keys = append(keys, k)
	}
	return &structKeyManager{keys: keys}
}

func (m *structKeyManager) current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.keys) == 0 {
		return ""
	}
	return m.keys[m.activeIdx]
}

// reportError increments the consecutive-error counter and rotates to the next key if the
// threshold is reached. Returns the new active key (empty if no keys configured).
func (m *structKeyManager) reportError(logFn func(string, string)) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.keys) == 0 {
		return ""
	}
	m.errCount++
	if m.errCount >= 3 && len(m.keys) > 1 {
		prev := m.activeIdx + 1
		m.activeIdx = (m.activeIdx + 1) % len(m.keys)
		m.errCount = 0
		logFn("WARN", fmt.Sprintf("[Struct Key Manager] Key #%d hit 3 consecutive errors — rotating to key #%d.", prev, m.activeIdx+1))
	}
	return m.keys[m.activeIdx]
}

func (m *structKeyManager) reportSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errCount = 0
}

type PolymarketEngine struct {
	store               *db.DB
	walletAddress       string // signer EOA address (derived from private key)
	funderAddress       string // Magic.link proxy wallet (holds USDC); empty for EOA accounts
	privateKey          *ecdsa.PrivateKey
	creds               *polymarketCredentials // non-nil when POLY_LIVE=true
	liveTrading         bool                   // true when POLY_LIVE=true and credentials loaded
	prices              map[string]float64
	slStates            map[string]time.Time // Track first breached timestamp for each position ID
	lastStopCloseTime   map[string]time.Time // condition-ID -> time of last stop-loss close (re-entry cooldown)
	rpcURL              string
	clobURL             string
	yesTokenID          string
	noTokenID           string
	negRiskMarket       bool // from Gamma API; selects the V2 exchange contract for signing
	activeMarketAddress string
	wsConn              *websocket.Conn
	structConn          *websocket.Conn // position-scoped Struct WS; nil when flat
	mu                  sync.RWMutex
	accountMu           sync.Mutex // Serializes wallet balance + position open/close (prevents double-close/double-refund races)
	structMu            sync.Mutex // Protects structConn lifecycle (connect on OpenPosition, close on ClosePosition)
	keyMgr              *structKeyManager
	bookCache           map[string]cachedBook // tokenID -> short-lived order-book snapshot (see fetchBook)
	bookCacheMu         sync.Mutex
	shadowRedemptions   map[string]*shadowEntry // posID -> pending TP/SL-vs-redemption comparison, see recordShadowFromClosed/checkShadowRedemptions
	shadowMu            sync.Mutex
	redeemAttempts      map[string]time.Time // posID -> last live-redemption attempt (dedupe/backoff, see startLiveRedemption)
	redeemMu            sync.Mutex
	nearMisses          map[string]*nearMissEntry // conditionID -> a sub-threshold setup we rejected, tracked to settlement (see RecordNearMiss)
	nearMissMu          sync.Mutex
}

// nearMissEntry records an entry the strategy REJECTED only because its edge
// fell in [0.06, 0.12) — i.e. it passed every other gate (price in band, book
// tight, probability not saturated) but missed the $0.12 edge floor. We never
// take the trade; we just track it to settlement so we can answer "is the 0.12
// floor too strict?" from real outcomes instead of a fragile after-the-fact
// log join. Pure analysis: no capital, no position, no balance effect.
type nearMissEntry struct {
	side       string // "YES" or "NO" — the side we would have bought
	edge       float64
	buyPrice   float64
	strike     float64
	expiryUnix int64
	recordedAt time.Time
}

// shadowEntry captures an early-exited (TP/SL/scalp-flatten) trade so that,
// once its contract's true expiry passes, we can log what holding to
// redemption instead would have paid — for direct comparison. Analysis only:
// never mutates balance or position state, since the real exit already
// happened.
type shadowEntry struct {
	instrument  string
	units       float64
	openPrice   float64
	strike      float64
	expiryUnix  int64
	actualClose float64
	actualPnl   float64
	closeReason string
}

func NewPolymarketEngine(store *db.DB, pkHex string, rpcURL string) (*PolymarketEngine, error) {
	// Attempt to load live credentials first. Falls back to paper mode if POLY_LIVE != "true".
	privKey, signerAddr, funderAddr, creds, err := loadLiveCredentials()
	if err != nil {
		return nil, fmt.Errorf("live credential setup failed: %w", err)
	}

	isLive := creds != nil
	environment := "demo"

	// walletAddr is used as the DB account key. In live mode use funder (real USDC wallet);
	// fall back to signer if POLY_FUNDER_ADDRESS wasn't set.
	walletAddr := signerAddr
	if isLive {
		environment = "live"
		if funderAddr != "" {
			walletAddr = funderAddr
		}
		store.Log("INFO", fmt.Sprintf("[Polymarket Engine] LIVE MODE active. Funder: %s | Signer: %s", walletAddr, signerAddr))
	} else {
		// Paper mode: use a deterministic mock address
		walletAddr = "0x71C7656EC7ab88b098defB751B7401B5f6d1476B"
		signerAddr = walletAddr
		store.Log("INFO", "[Polymarket Engine] PAPER MODE (set POLY_LIVE=true to enable live trading).")
	}

	// Initialize local wallet account for balance tracking
	accID := "polymarket_wallet_" + walletAddr
	_, err = store.GetAccount(accID)
	if err != nil {
		startBal := 100.00
		if isLive {
			startBal = 0 // Will be synced from CLOB balance on first GetBalance call
		}
		err = store.SaveAccount(db.Account{
			ID:          accID,
			Environment: environment,
			Balance:     startBal,
			Currency:    "USDC",
			UpdatedAt:   time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Polymarket wallet account: %w", err)
		}
		store.Log("INFO", fmt.Sprintf("Web3 Polymarket Wallet initialized. Address: %s", walletAddr))
	}

	km := newStructKeyManager()
	if len(km.keys) == 0 {
		store.Log("WARN", "[Struct Key Manager] No STRUCT_API_KEY configured — liquidity checks will fail-open.")
	} else {
		store.Log("INFO", fmt.Sprintf("[Struct Key Manager] Loaded %d API key(s).", len(km.keys)))
	}

	engine := &PolymarketEngine{
		store:             store,
		walletAddress:     signerAddr, // signer EOA (signs orders, L1/L2 auth)
		funderAddress:     funderAddr, // proxy wallet (holds USDC, set as maker in orders)
		privateKey:        privKey,
		creds:             creds,
		liveTrading:       isLive,
		prices:            make(map[string]float64),
		slStates:          make(map[string]time.Time),
		lastStopCloseTime: make(map[string]time.Time),
		bookCache:         make(map[string]cachedBook),
		shadowRedemptions: make(map[string]*shadowEntry),
		redeemAttempts:    make(map[string]time.Time),
		nearMisses:        make(map[string]*nearMissEntry),
		rpcURL:            rpcURL,
		clobURL:           "https://clob.polymarket.com",
		keyMgr:            km,
	}

	// Sync real CLOB balance on startup (live only)
	if isLive {
		if bal, err := engine.getCLOBBalance(); err == nil {
			accID := "polymarket_wallet_" + walletAddr
			if acc, err := store.GetAccount(accID); err == nil {
				acc.Balance = bal
				acc.UpdatedAt = time.Now()
				_ = store.SaveAccount(acc)
				store.Log("INFO", fmt.Sprintf("[Polymarket Engine] CLOB balance synced: $%.2f USDC", bal))
			}
		} else {
			store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Could not sync CLOB balance: %v", err))
		}
	}

	// Start the real-time WebSocket connection to Polymarket CLOB
	go engine.StartWSListener()

	// Dynamic trigger evaluator loop for Polymarket positions
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			_ = engine.EvaluatePositionTriggers()
			engine.checkShadowRedemptions()
			engine.checkNearMisses()
		}
	}()

	return engine, nil
}

func (p *PolymarketEngine) GetBalance() (float64, float64, error) {
	accID := "polymarket_wallet_" + p.accountKey()
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

// GetPosition returns a single position by ID (passthrough to the store). Used
// by S3's post-fill hedge sanity check.
func (p *PolymarketEngine) GetPosition(id string) (db.Position, error) {
	return p.store.GetPosition(id)
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

	accID := "polymarket_wallet_" + p.accountKey()
	acc, err := p.store.GetAccount(accID)
	if err != nil {
		return "", err
	}

	cost := math.Abs(units) * currentPrice
	if acc.Balance < cost {
		return "", fmt.Errorf("insufficient USDC balance in Web3 wallet: have %.2f, need %.2f", acc.Balance, cost)
	}

	// Determine which token we're buying
	tokenID := yToken
	if units < 0 {
		tokenID = nToken
	}

	if p.liveTrading {
		// Submit real FOK buy order to Polymarket CLOB. The order is sized to
		// whole shares, so replace the strategy's fractional units with the
		// actual filled quantity (keeping the YES/NO sign) and deduct the real
		// USDC spend — otherwise the later sell would try to move shares we
		// never owned and the balance tracker would drift from the wallet.
		limitPrice := currentPrice
		orderID, filledShares, actualCost, err := p.submitLiveBuyOrder(tokenID, currentPrice, cost)
		if err != nil {
			return "", fmt.Errorf("[CLOB] Buy order failed: %w", err)
		}
		if units < 0 {
			units = -filledShares
		} else {
			units = filledShares
		}
		cost = actualCost
		// Price the position at the REAL average fill, not the limit we asked
		// for — a marketable FOK can fill better than its limit (see
		// submitLiveBuyOrder). This becomes pos.OpenPrice below: the cost basis
		// every downstream PnL calc (ClosePosition, ResolvePosition) is computed
		// against. Leaving it at the stale limit would silently mis-price every
		// fill that got price improvement — the buy-side twin of the sell-side
		// bug already fixed in submitLiveSellOrder/ClosePosition.
		if filledShares > 0 {
			currentPrice = actualCost / filledShares
		}
		p.store.Log("INFO", fmt.Sprintf("[Web3 CLOB] LIVE buy order filled. OrderID: %s | Token: %s | Price: $%.4f (limit was $%.2f) | Shares: %.0f | USDC: $%.2f",
			orderID, tokenID[:8]+"...", currentPrice, limitPrice, filledShares, actualCost))
	} else {
		p.store.Log("INFO", fmt.Sprintf("[Web3 CLOB] Signed EIP-712 buy order for %s outcome. Wallet: %s", market, p.walletAddress))
	}

	// Taker fee on the entry leg (see cryptoTakerFeeRate) — computed from the final
	// units so a live fill's actual filled size is what gets charged, not the estimate.
	entryFee := takerFee(math.Abs(units), currentPrice)

	// Deduct USDC from local balance tracker (both live and paper)
	acc.Balance -= cost
	acc.Balance -= entryFee
	acc.UpdatedAt = time.Now()
	_ = p.store.SaveAccount(acc)

	// Save position to local DB (nanotime + 4 random bytes = collision-proof ID)
	randSuffix := make([]byte, 4)
	_, _ = crand.Read(randSuffix)
	posID := fmt.Sprintf("poly_%d_%s", time.Now().UnixNano(), hex.EncodeToString(randSuffix))
	pos := db.Position{
		ID:         posID,
		Instrument: market,
		Units:      units,
		OpenPrice:  currentPrice,
		OpenTime:   time.Now(),
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Status:     "OPEN",
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
	if p.keyMgr.current() != "" && yToken != "" && nToken != "" {
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

	if p.liveTrading {
		tokenID := p.resolveTokenForClose(pos.Units)
		if tokenID == "" {
			// Token IDs not yet known (e.g. mid-restart). Keep position OPEN to avoid
			// inflating local balance for a sell that never happened on-chain.
			p.store.Log("ERROR", fmt.Sprintf("[CLOB] Cannot sell %s: token IDs unknown (WS not yet subscribed). Position kept OPEN.", id))
			return fmt.Errorf("CLOB sell skipped: token IDs unavailable for %s", id)
		}

		// Price the exit at the live best bid (marketable) so the FOK actually
		// crosses — a stop-loss/TP sell limited at the trigger price frequently
		// sits above the bid and gets killed, leaving the position stuck open.
		// Getting OUT is the priority, so normal stop slippage is accepted.
		//
		// BUT cap the slippage: never sell more than maxExitSlippage below the
		// trigger. If the bid has collapsed past the floor, we price at the floor
		// instead — the FOK won't fill, the position stays open and retries,
		// bounding the per-attempt loss rather than dumping at the bottom of a
		// thin/transient dip. The 90s flatten passes the live price as its own
		// trigger, so its floor sits below the bid and it still exits cleanly.
		shares := math.Abs(pos.Units)
		isYes := pos.Units > 0
		floorPrice := currentPrice - maxExitSlippage
		sellPrice := currentPrice
		if bid, fillable, mErr := p.GetMarketablePrice(isYes, 1, shares); mErr == nil && fillable {
			if bid < floorPrice {
				sellPrice = floorPrice
				p.store.Log("INFO", fmt.Sprintf("[CLOB] Exit %s: best bid $%.2f is >$%.2f below trigger $%.2f — capping sell at floor $%.2f (won't dump the bottom; will retry).", id, bid, maxExitSlippage, currentPrice, floorPrice))
			} else {
				sellPrice = bid
				if bid < currentPrice {
					p.store.Log("INFO", fmt.Sprintf("[CLOB] Exit %s: trigger $%.2f above best bid — selling marketable at $%.2f.", id, currentPrice, bid))
				}
			}
		} else {
			p.store.Log("WARN", fmt.Sprintf("[CLOB] Exit %s: live bid unavailable (fillable=%t, err=%v) — using trigger price $%.2f.", id, fillable, mErr, currentPrice))
		}

		_, proceeds, sellErr := p.submitLiveSellOrder(tokenID, sellPrice, shares)
		if sellErr != nil {
			// CRITICAL: do NOT close locally — that would inflate the balance with USDC we never received.
			p.store.Log("ERROR", fmt.Sprintf("[CLOB] SELL order for %s FAILED: %v — position kept OPEN to prevent balance inflation.", id, sellErr))
			return fmt.Errorf("CLOB sell failed: %w", sellErr)
		}
		// Price the exit off the ACTUAL proceeds, not the pre-trade estimate — a
		// marketable FOK can fill better than its limit (see submitLiveSellOrder).
		// Dividing the real proceeds by the same `shares` used in the PnL math
		// below keeps the two internally consistent regardless of any float
		// jitter between this estimate and the whole-share amount actually sold.
		estPrice := sellPrice
		currentPrice = proceeds / shares
		p.store.Log("INFO", fmt.Sprintf("[Web3 CLOB] LIVE sell filled. Token: %s | Price: $%.4f (est was $%.2f) | Shares: %.2f | Proceeds: $%.2f",
			tokenID[:8]+"...", currentPrice, estPrice, shares, proceeds))
	}

	// Realized P&L against the actual close price (marketable fill in live mode),
	// net of taker fees on both legs. Entry fee isn't stored on the position record;
	// it's cheap and exact to recompute from pos.OpenPrice/pos.Units, which we
	// already have — see cryptoTakerFeeRate.
	shares := math.Abs(pos.Units)
	grossPnl := shares * (currentPrice - pos.OpenPrice)
	entryFee := takerFee(shares, pos.OpenPrice)
	exitFee := takerFee(shares, currentPrice)
	pnl := grossPnl - entryFee - exitFee

	// Refund balance + returns to Web3 wallet (local tracker), net of the exit fee.
	accID := "polymarket_wallet_" + p.accountKey()
	acc, err := p.store.GetAccount(accID)
	if err == nil {
		payoutAmount := shares*currentPrice - exitFee
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

	p.store.Log("INFO", fmt.Sprintf("[Polymarket] Sold %.2f shares of %s early at $%.2f USDC/share. Realized PnL: $%.2f USDC (gross $%.2f, fees $%.2f)", shares, pos.Instrument, currentPrice, pnl, grossPnl, entryFee+exitFee))

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

// ResolvePosition closes a position via contract SETTLEMENT (resolutionPrice is
// exactly 0.0 or 1.0), not an active CLOB sell. Redemption is free — confirmed
// against real account activity (a REDEEM entry's usdcSize exactly equalled its
// share size, no fee deducted) — so unlike ClosePosition this charges the entry
// fee (already paid at open) but NOT a fabricated exit fee.
//
// This function only updates local bookkeeping. In paper mode the expiry
// branch calls it directly; in live mode LiveRedeemPosition (polymarket_redeem.go)
// calls it only AFTER the on-chain CTF redemption is confirmed (winners) or the
// on-chain resolution shows we lost (no tx needed, tokens are worthless).
func (p *PolymarketEngine) ResolvePosition(id string, resolutionPrice float64) error {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()

	pos, err := p.store.GetPosition(id)
	if err != nil {
		return err
	}
	if pos.Status == "CLOSED" {
		return nil
	}

	shares := math.Abs(pos.Units)
	grossPnl := shares * (resolutionPrice - pos.OpenPrice)
	entryFee := takerFee(shares, pos.OpenPrice)
	pnl := grossPnl - entryFee // no exit fee — redemption is free

	accID := "polymarket_wallet_" + p.accountKey()
	acc, err := p.store.GetAccount(accID)
	if err == nil {
		acc.Balance += shares * resolutionPrice // redemption payout, no fee
		acc.UpdatedAt = time.Now()
		_ = p.store.SaveAccount(acc)
	}

	now := time.Now()
	pos.Status = "CLOSED"
	pos.ClosePrice = &resolutionPrice
	pos.CloseTime = &now
	pos.RealizedPnL = &pnl
	_ = p.store.SavePosition(pos)

	_ = p.store.SaveTransaction(db.Transaction{
		ID:          "tx_close_" + pos.ID,
		Type:        "REDEEM",
		Instrument:  pos.Instrument,
		Price:       resolutionPrice,
		Units:       pos.Units,
		RealizedPnL: pnl,
		Timestamp:   time.Now(),
	})

	p.store.Log("INFO", fmt.Sprintf("[Polymarket] Redeemed %.2f shares of %s at $%.2f/share (settlement). Realized PnL: $%.2f (gross $%.2f, entry fee $%.2f, no exit fee)",
		shares, pos.Instrument, resolutionPrice, pnl, grossPnl, entryFee))

	p.mu.Lock()
	delete(p.slStates, id)
	p.mu.Unlock()

	p.structMu.Lock()
	if p.structConn != nil {
		p.structConn.Close()
		p.structConn = nil
	}
	p.structMu.Unlock()

	return nil
}

// recordShadowFromClosed re-reads a just-closed position from the store (to get
// the authoritative fill price/PnL, including any live-mode slippage
// adjustment ClosePosition applied) and files it for the TP/SL-vs-redemption
// comparison logged once its contract truly expires. See checkShadowRedemptions.
func (p *PolymarketEngine) recordShadowFromClosed(posID, instrument string, units, openPrice, strike float64, expiryUnix int64, reason string) {
	closed, err := p.store.GetPosition(posID)
	if err != nil || closed.ClosePrice == nil || closed.RealizedPnL == nil {
		return
	}
	p.shadowMu.Lock()
	p.shadowRedemptions[posID] = &shadowEntry{
		instrument:  instrument,
		units:       units,
		openPrice:   openPrice,
		strike:      strike,
		expiryUnix:  expiryUnix,
		actualClose: *closed.ClosePrice,
		actualPnl:   *closed.RealizedPnL,
		closeReason: reason,
	}
	p.shadowMu.Unlock()
}

// checkShadowRedemptions logs the TP/SL-vs-redemption comparison for any
// early-exited trade whose contract has now actually expired, using the same
// spot-vs-strike settlement rule as ResolvePosition. Pure logging: no balance
// or position mutation — the real exit already happened at record time.
func (p *PolymarketEngine) checkShadowRedemptions() {
	now := time.Now().Unix()

	p.shadowMu.Lock()
	var due []string
	for id, e := range p.shadowRedemptions {
		if now >= e.expiryUnix {
			due = append(due, id)
		}
	}
	p.shadowMu.Unlock()

	for _, id := range due {
		p.shadowMu.Lock()
		e, ok := p.shadowRedemptions[id]
		if ok {
			delete(p.shadowRedemptions, id)
		}
		p.shadowMu.Unlock()
		if !ok {
			continue
		}

		spotPrice, hasSpot := p.GetPrice("BTC_USD")
		if !hasSpot {
			p.store.Log("WARN", fmt.Sprintf("[Shadow Redemption] pos=%s: no spot price available at expiry — skipping comparison.", id))
			continue
		}

		isLong := e.units > 0
		won := (isLong && spotPrice >= e.strike) || (!isLong && spotPrice < e.strike)
		resolutionPrice := 0.0
		if won {
			resolutionPrice = 1.0
		}

		shares := math.Abs(e.units)
		shadowGross := shares * (resolutionPrice - e.openPrice)
		entryFee := takerFee(shares, e.openPrice)
		shadowPnl := shadowGross - entryFee // redemption is free — no exit fee

		diff := shadowPnl - e.actualPnl
		better := "early exit"
		if diff > 0 {
			better = "redemption"
		}
		p.store.Log("INFO", fmt.Sprintf(
			"[Shadow Redemption] pos=%s reason=%s | actual: closed @ $%.3f pnl=$%.3f | hold-to-redemption: spot $%.2f vs strike $%.2f -> settle $%.2f pnl=$%.3f | diff=$%.3f (%s would have been better)",
			id, e.closeReason, e.actualClose, e.actualPnl, spotPrice, e.strike, resolutionPrice, shadowPnl, diff, better))
	}
}

// RecordNearMiss files a sub-threshold setup (edge just under the floor, but
// otherwise tradeable) to be scored at settlement. Deduped to one per contract
// (the first near-miss seen), matching how the real strategy takes at most one
// entry per contract. See nearMissEntry.
func (p *PolymarketEngine) RecordNearMiss(instrument, side string, edge, buyPrice, strike float64, expiryUnix int64) {
	cond := conditionID(instrument)
	p.nearMissMu.Lock()
	defer p.nearMissMu.Unlock()
	if _, exists := p.nearMisses[cond]; exists {
		return
	}
	p.nearMisses[cond] = &nearMissEntry{
		side:       side,
		edge:       edge,
		buyPrice:   buyPrice,
		strike:     strike,
		expiryUnix: expiryUnix,
		recordedAt: time.Now(),
	}
}

// checkNearMisses scores any recorded near-miss whose contract has expired,
// logging what taking it would have returned (hold-to-settlement, entry fee
// only). Same spot-vs-strike settlement rule as ResolvePosition. Pure logging;
// prunes stale entries so the map can't grow unbounded.
func (p *PolymarketEngine) checkNearMisses() {
	now := time.Now().Unix()

	p.nearMissMu.Lock()
	var due []string
	for cond, e := range p.nearMisses {
		if now >= e.expiryUnix {
			due = append(due, cond)
		} else if now-e.expiryUnix > 600 || (e.expiryUnix == 0 && time.Since(e.recordedAt) > 10*time.Minute) {
			delete(p.nearMisses, cond) // stale/malformed — drop
		}
	}
	p.nearMissMu.Unlock()

	for _, cond := range due {
		p.nearMissMu.Lock()
		e, ok := p.nearMisses[cond]
		if ok {
			delete(p.nearMisses, cond)
		}
		p.nearMissMu.Unlock()
		if !ok {
			continue
		}

		spot, hasSpot := p.GetPrice("BTC_USD")
		if !hasSpot {
			p.store.Log("WARN", fmt.Sprintf("[Near-Miss] cond=%s: no spot at settlement — cannot score.", cond[:min(10, len(cond))]))
			continue
		}
		yesWon := spot >= e.strike
		won := (e.side == "YES" && yesWon) || (e.side == "NO" && !yesWon)
		shares := 4.0 / e.buyPrice
		payout := 0.0
		if won {
			payout = 1.0
		}
		entryFee := takerFee(shares, e.buyPrice)
		settleNet := (payout-e.buyPrice)*shares - entryFee
		outcome := "LOST"
		if won {
			outcome = "WON"
		}
		p.store.Log("INFO", fmt.Sprintf(
			"[Near-Miss] cond=%s side=%s edge=$%.3f buy=$%.3f | settlement: spot $%.2f vs strike $%.2f -> our side %s | if-taken(hold-to-settle) net=$%.3f",
			cond[:min(10, len(cond))], e.side, e.edge, e.buyPrice, spot, e.strike, outcome, settleNet))
	}
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
	if p.liveTrading {
		return "live"
	}
	return "demo"
}

// StrategyEnabled reports whether a strategy tag ("s1"/"s2"/"s3") should place
// trades this tick. In PAPER mode (POLY_LIVE != true) everything runs. In LIVE
// mode ONLY the strategies named in the LIVE_STRATEGIES env var run — so a
// single proven strategy (e.g. LIVE_STRATEGIES=s3) can go to real money while
// the rest stay parked. This is a hard gate at the entry point, and it also
// avoids a paper/live balance-tracker collision: with S1/S2 gated off in live
// mode, only the live strategy touches the (real-balance-synced) wallet
// account. Fail-safe: live mode with an empty allowlist trades NOTHING.
func (p *PolymarketEngine) StrategyEnabled(tag string) bool {
	// Hard kill-list, honored in BOTH paper and live — used to fully retire a
	// dead strategy (e.g. S3, confirmed unfillable) so it stops polluting the
	// paper sample. Set DISABLED_STRATEGIES=s3 (comma-separated) in .env.
	for _, t := range strings.Split(os.Getenv("DISABLED_STRATEGIES"), ",") {
		if strings.EqualFold(strings.TrimSpace(t), tag) {
			return false
		}
	}
	if !p.liveTrading {
		return true
	}
	allow := os.Getenv("LIVE_STRATEGIES")
	for _, t := range strings.Split(allow, ",") {
		if strings.EqualFold(strings.TrimSpace(t), tag) {
			return true
		}
	}
	return false
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

	apiKey := p.keyMgr.current()
	if apiKey == "" {
		p.store.Log("WARN", "[Polymarket Engine] No STRUCT_API_KEY configured — liquidity depth UNVERIFIED, allowing entry (fail-open).")
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
		p.keyMgr.reportError(p.store.Log)
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Struct order-book call failed: %v. Skipping entry (fail-closed).", err))
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.keyMgr.reportError(p.store.Log)
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Struct order-book returned HTTP %d. Skipping entry (fail-closed).", resp.StatusCode))
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
		p.keyMgr.reportError(p.store.Log)
		p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] Failed to decode order book JSON: %v. Skipping entry (fail-closed).", err))
		return false, nil
	}

	// Successful API round-trip — reset error counter.
	p.keyMgr.reportSuccess()

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
		// Force-close 90s before expiry, priced from the live book (same reasoning as
		// the SL/TP check below: the tape can misstate the position's real value).
		// Skipped for hold-to-redemption positions (instrument tagged "_s2") — those
		// are DESIGNED to ride into settlement and redeem for free; forcing them out
		// here would defeat the entire strategy. See ResolvePosition.
		// _s2: late-window decisive hold. _s3y/_s3n: the two legs of a
		// dislocation-arb pair — both MUST reach settlement (exactly one pays
		// $1; flattening either leg early would unhedge the pair). _s4: final-30s
		// favorite hold — enters INSIDE this 90s window by design, so without
		// this exemption every S4 position would be force-flattened the tick
		// after it opened.
		holdToRedemption := strings.HasSuffix(pos.Instrument, "_s2") ||
			strings.HasSuffix(pos.Instrument, "_s3y") || strings.HasSuffix(pos.Instrument, "_s3n") ||
			strings.HasSuffix(pos.Instrument, "_s4")
		if !holdToRedemption && expiryUnix-time.Now().Unix() <= 90 {
			p.mu.RLock()
			yesPx, hasPx := p.prices[priceKey(pos.Instrument)]
			p.mu.RUnlock()
			if hasPx {
				closePx := yesPx
				if pos.Units < 0 {
					closePx = 1.0 - yesPx
				}
				if bp, ok, err := p.GetMarketablePrice(pos.Units > 0, 1, math.Abs(pos.Units)); err == nil && ok {
					closePx = bp
				}
				p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Scalp flatten: %ds to expiry (<=90s). Closing %s at $%.3f to avoid settlement risk.", expiryUnix-time.Now().Unix(), pos.ID, closePx))
				p.mu.Lock()
				delete(p.slStates, pos.ID)
				p.mu.Unlock()
				_ = p.ClosePosition(pos.ID, closePx)
				p.recordShadowFromClosed(pos.ID, pos.Instrument, pos.Units, pos.OpenPrice, strike, expiryUnix, "SCALP_FLATTEN")
				continue
			}
		}

		// 1. Expiration check: check if expiration time is reached
		if time.Now().Unix() >= expiryUnix {
			if p.liveTrading {
				// Live mode: settlement is an on-chain event, not local bookkeeping.
				// Route through the CTF redemption flow, which waits for the oracle,
				// redeems winners from the proxy wallet, and only then books the
				// settlement (losers book $0 without spending gas). Deduped +
				// backoff internally, so calling every tick is safe.
				p.startLiveRedemption(pos)
				continue
			}
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

			_ = p.ResolvePosition(pos.ID, resolutionPrice)
			continue
		}

		// S2 reversal bail-out: a hold-to-redemption position is only supposed to
		// ride to settlement while Kraken spot still favors the side we bought. If
		// spot re-crosses the strike, sell immediately instead of risking a $0
		// redemption — our own data shows BTC can reverse meaningfully even with
		// under a minute left (one contract went from 86% one way to 29% the other
		// within two minutes), so "looked decided at entry" is not a guarantee.
		// Bail-out applies ONLY to S2 (a directional hold). S3 pair legs are
		// hedged — one leg is ALWAYS "on the wrong side of the strike" by
		// construction, and bailing it would unhedge the locked profit.
		//
		// Two triggers, whichever fires first:
		//  (a) PRICE STOP — the held side's sellable price falls to
		//      s2BailTokenPrice ($0.50). This CAPS the loss at ~(entry-0.50)/share
		//      instead of riding a reversal to a $0 redemption. Priced off the
		//      real book (GetMarketablePrice), never the noisy tape.
		//  (b) SPOT REVERSAL — BTC re-crosses the strike (backstop for the case
		//      where the book is stale but the underlying has clearly flipped).
		if holdToRedemption && strings.HasSuffix(pos.Instrument, "_s2") {
			isLong := pos.Units > 0
			shares := math.Abs(pos.Units)
			tokenPx, pxOk, _ := p.GetMarketablePrice(isLong, 1, shares)
			spotPrice, hasSpot := p.GetPrice("BTC_USD")
			spotReversed := hasSpot && !((isLong && spotPrice >= strike) || (!isLong && spotPrice < strike))
			hitStop := pxOk && tokenPx <= s2BailTokenPrice
			if hitStop || spotReversed {
				bailPrice := tokenPx
				if !pxOk {
					p.mu.RLock()
					yesPx, hasPx := p.prices[priceKey(pos.Instrument)]
					p.mu.RUnlock()
					if hasPx {
						bailPrice = yesPx
						if !isLong {
							bailPrice = 1.0 - yesPx
						}
					}
				}
				reason := fmt.Sprintf("spot $%.2f reversed across strike $%.2f", spotPrice, strike)
				if hitStop {
					reason = fmt.Sprintf("token hit $%.2f price stop", s2BailTokenPrice)
				}
				p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] S2 bail-out (%s) for %s. Selling at $%.3f to cap the loss instead of riding to a $0 redemption.", reason, pos.ID, bailPrice))
				_ = p.ClosePosition(pos.ID, bailPrice)
				continue
			}
		}

		// S4 bail-out. S4 buys whichever side the BOOK favors in the final 30s and
		// holds blind to redemption. Two triggers, whichever fires first — mirroring
		// the S2 bail structure above:
		//
		//  (a) SIDE FLIP — the side we hold is no longer the market's favorite
		//      (its mid has fallen below the other side's). This is the exact
		//      negation of S4's own entry rule, so it needs no tuned threshold:
		//      we entered because our side led, we leave when it stops leading.
		//
		//  (b) SPOT REVERSAL (backstop) — BTC has crossed to the wrong side of
		//      the strike. Covers the case where the book is unreadable/stale,
		//      and independently catches the failure mode that produced both
		//      real live losses: the book favored one side while our own spot
		//      feed said the opposite, and spot was right both times.
		//
		// Sells at the live bid via ClosePosition. Note this fires only BEFORE
		// expiry (the expiry branch above `continue`s), so it can never price
		// against the next contract's book after token IDs rotate.
		if holdToRedemption && strings.HasSuffix(pos.Instrument, "_s4") {
			isLong := pos.Units > 0

			sideFlipped := false
			if hBid, hAsk, okH := p.GetTopOfBook(isLong); okH {
				if oBid, oAsk, okO := p.GetTopOfBook(!isLong); okO {
					sideFlipped = (hBid+hAsk)/2 < (oBid+oAsk)/2
				}
			}

			spotPrice, hasSpot := p.GetPrice("BTC_USD")
			spotReversed := hasSpot && !((isLong && spotPrice >= strike) || (!isLong && spotPrice < strike))

			if sideFlipped || spotReversed {
				bailPrice, pxOk, _ := p.GetMarketablePrice(isLong, 1, math.Abs(pos.Units))
				if !pxOk {
					p.mu.RLock()
					yesPx, hasPx := p.prices[priceKey(pos.Instrument)]
					p.mu.RUnlock()
					if hasPx {
						bailPrice = yesPx
						if !isLong {
							bailPrice = 1.0 - yesPx
						}
					}
				}
				reason := fmt.Sprintf("spot $%.2f crossed strike $%.2f", spotPrice, strike)
				if sideFlipped {
					reason = "held side lost the lead (side flip)"
				}
				p.store.Log("WARN", fmt.Sprintf("[Polymarket Engine] S4 bail-out (%s) for %s. Selling at $%.3f rather than riding to a $0 settlement.", reason, pos.ID, bailPrice))
				_ = p.ClosePosition(pos.ID, bailPrice)
				continue
			}
		}

		// Fetch the tape price as a last-resort fallback only (see below).
		key := priceKey(pos.Instrument)
		p.mu.RLock()
		yesPrice, exists := p.prices[key]
		p.mu.RUnlock()

		if !exists {
			continue
		}

		// Price the position against the LIVE ORDER BOOK, not the raw trade tape — a
		// single $1-2 print can swing the tape 10+ cents in a couple of seconds without
		// the real book moving at all (confirmed after a run of sub-second "stop-loss"
		// closes that fired on tape noise while the book itself sat stable). This is the
		// exact same call ClosePosition uses to price the actual exit, so the decision to
		// close and the price we get now agree, instead of triggering on one source and
		// executing against another.
		shares := math.Abs(pos.Units)
		isYes := pos.Units > 0
		bookPrice, fillable, bookErr := p.GetMarketablePrice(isYes, 1, shares) // side=1: SELL/bids
		if bookErr != nil || !fillable {
			// Book unreachable or too thin to price our size — fall back to the tape
			// rather than skip the check (still better than no SL protection at all).
			bookPrice = yesPrice
			if pos.Units < 0 {
				bookPrice = 1.0 - yesPrice
			}
		}

		// The price of our owned share token, used for both TP and SL checks.
		price := bookPrice
		slPrice := bookPrice

		// 2. Stop Loss check: close immediately when price breaches the trigger.
		// No confirmation timer — a 3-cent scalp stop has no room for delay; waiting
		// 1 second on a fast binary token means closing $0.10-$0.40 below the intended
		// trigger, turning a $0.30 controlled loss into a $1-2 loss.
		// Close at slTrigger (the intended stop price), not slPrice (current price),
		// so the realized loss is capped at the defined risk even on gap-downs.
		slTrigger := pos.StopLoss
		if slTrigger <= 0 {
			// No valid SL configured — skip rather than defaulting to OpenPrice,
			// which would trigger an immediate close on any tiny pullback.
			goto checkTP
		}

		if slPrice <= slTrigger {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Stop Loss triggered (Price: $%.4f <= Trigger: $%.4f). Closing at SL trigger price.", slPrice, slTrigger))
			p.mu.Lock()
			delete(p.slStates, pos.ID)
			// Start the re-entry cooldown for this contract (see InCooldown): a stop-out
			// often means a directional move is still running, and re-entering
			// immediately just buys back into the same move (observed 6 stop-outs on
			// one contract in ~15s before this was added).
			p.lastStopCloseTime[cooldownKey(pos.Instrument)] = time.Now()
			p.mu.Unlock()
			_ = p.ClosePosition(pos.ID, slTrigger)
			p.recordShadowFromClosed(pos.ID, pos.Instrument, pos.Units, pos.OpenPrice, strike, expiryUnix, "SL")
			continue
		}

		// 3. Take Profit check: if current token price meets or exceeds target predicted probability, sell/exit early to lock in profits
	checkTP:
		if pos.TakeProfit > 0 && price >= pos.TakeProfit {
			p.store.Log("INFO", fmt.Sprintf("[Polymarket Engine] Take Profit triggered (Current Share Price: $%.2f >= Predicted Target: $%.2f). Locking in profit.", price, pos.TakeProfit))
			p.mu.Lock()
			delete(p.slStates, pos.ID) // Clear any SL states
			p.mu.Unlock()
			_ = p.ClosePosition(pos.ID, price)
			p.recordShadowFromClosed(pos.ID, pos.Instrument, pos.Units, pos.OpenPrice, strike, expiryUnix, "TP")
			continue
		}
	}

	return nil
}

// accountKey returns the address used as the local DB account identifier.
// In live mode this is the funder (proxy wallet that holds USDC); in paper mode it's the mock address.
func (p *PolymarketEngine) accountKey() string {
	if p.funderAddress != "" {
		return p.funderAddress
	}
	return p.walletAddress
}

// Utility helper
func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// maxExitSlippage caps how far below the exit trigger a stop/TP sell will chase the
// bid. Beyond this the FOK is priced at the floor (usually won't fill) so the position
// holds and retries rather than dumping into a collapsing/transient-thin book.
const maxExitSlippage = 0.10

// s2BailTokenPrice is the price stop for a held S2 (buffer-strategy) position:
// if the side we bought can only be sold at or below this, cut it — capping the
// loss at ~(entry-0.50)/share rather than riding a reversal to a $0 redemption.
// See the S2 bail-out in EvaluatePositionTriggers.
const s2BailTokenPrice = 0.50

// cryptoTakerFeeRate is Polymarket's documented taker fee rate for the Crypto
// market category (our BTC Up/Down contracts): fee = shares * rate * price *
// (1-price), a parabolic curve peaking at price=$0.50. Only takers pay this —
// makers (resting limit orders) are free — but both our entry (marketable buy)
// and exit (marketable sell via GetMarketablePrice) are deliberately designed
// to cross the spread for reliable fills, so we pay it on every leg of every
// trade. Verified against real trade data: paper PnL without this was +$0.29
// over our first 13 trades; with real fees applied, -$4.62. Modeling it here
// (paper and live share this code path) so paper-mode PnL reflects reality
// instead of silently assuming zero fees.
const cryptoTakerFeeRate = 0.07

func takerFee(shares, price float64) float64 {
	return shares * cryptoTakerFeeRate * price * (1 - price)
}

// reentryCooldown blocks re-entering the same contract for this long after a
// stop-loss close. A stop-out is often a sign the underlying move is still
// running; re-entering the same contract seconds later just buys back into it.
// Observed: 6 stop-outs on one contract in ~15s (-$2.34) before this existed.
const reentryCooldown = 25 * time.Second

// conditionID extracts the stable per-contract identifier from an instrument
// string ("poly_<condID>_strike_..._expiry_..."), so cooldown/dedup checks
// survive the strike value drifting slightly between ticks (same pattern used
// elsewhere to match "is this the same contract" — see strategy Tick's
// currentCondID).
func conditionID(instrument string) string {
	parts := strings.Split(instrument, "_")
	if len(parts) >= 2 {
		return parts[1]
	}
	return instrument
}

// cooldownKey is like conditionID but ALSO includes any trailing strategy/variant
// suffix (parts beyond the expiry at index 5 — e.g. "_v05", "_s2"), so the
// re-entry cooldown is tracked PER VARIANT. The three parallel S1 threshold
// variants (v05/v08/v12) trade the same contract simultaneously; without this,
// one variant stopping out would suppress re-entry for the others and
// contaminate the A/B/C comparison. Kept separate from conditionID because the
// redemption path needs the pure on-chain condition hex.
func cooldownKey(instrument string) string {
	parts := strings.Split(instrument, "_")
	if len(parts) < 2 {
		return instrument
	}
	key := parts[1]
	if len(parts) >= 7 {
		key += "_" + strings.Join(parts[6:], "_")
	}
	return key
}

// InCooldown reports whether the contract behind `instrument` is still within
// the post-stop-loss re-entry cooldown, and how much time remains.
func (p *PolymarketEngine) InCooldown(instrument string) (remaining time.Duration, active bool) {
	p.mu.RLock()
	t, ok := p.lastStopCloseTime[cooldownKey(instrument)]
	p.mu.RUnlock()
	if !ok {
		return 0, false
	}
	elapsed := time.Since(t)
	if elapsed >= reentryCooldown {
		return 0, false
	}
	return reentryCooldown - elapsed, true
}

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

func (p *PolymarketEngine) SubscribeToMarketTokens(yesToken, noToken, marketAddr string, negRisk bool) {
	activeKey := priceKey(marketAddr)

	p.mu.Lock()
	oldYes := p.yesTokenID
	oldNo := p.noTokenID
	p.yesTokenID = yesToken
	p.noTokenID = noToken
	p.negRiskMarket = negRisk
	p.activeMarketAddress = marketAddr

	// Bound the price maps: drop stale poly_ keys from prior contracts (positions never
	// survive past their own contract thanks to the 90s flatten), keeping only the active
	// contract and any non-poly keys (e.g. spot "BTC_USD").
	for k := range p.prices {
		if strings.HasPrefix(k, "poly_") && k != activeKey {
			delete(p.prices, k)
		}
	}
	// Same bound for the re-entry cooldown map: an entry past reentryCooldown can never
	// affect InCooldown again, so it's safe (and keeps this from growing for the life
	// of the process — a new contract rolls around every ~5 minutes, forever).
	for k, t := range p.lastStopCloseTime {
		if time.Since(t) > reentryCooldown {
			delete(p.lastStopCloseTime, k)
		}
	}

	conn := p.wsConn
	p.mu.Unlock()

	// Bound the shadow-redemption map: a shadow entry should always be drained by
	// checkShadowRedemptions within a few seconds of its expiry, but if spot price
	// is ever unavailable at that moment it stays pending forever — drop anything
	// stale enough that it can no longer be a useful comparison anyway.
	p.shadowMu.Lock()
	for id, e := range p.shadowRedemptions {
		if time.Now().Unix()-e.expiryUnix > 300 {
			delete(p.shadowRedemptions, id)
		}
	}
	p.shadowMu.Unlock()

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
	apiKey := p.keyMgr.current()
	if apiKey == "" {
		return
	}

	wsURL := fmt.Sprintf("wss://api.struct.to/ws?api-key=%s", apiKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		p.keyMgr.reportError(p.store.Log)
		p.store.Log("WARN", fmt.Sprintf("[Struct Position Feed] Connect failed: %v. CLOB feed remains sole monitor.", err))
		return
	}
	p.keyMgr.reportSuccess()

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
	p.mu.Unlock()

	p.store.Log("INFO", fmt.Sprintf("[Struct WS] Executed Trade print: %s outcome at $%.3f (Size: $%.2f USDC, Side: %s)",
		outcome, price, trade.UsdAmount, trade.Side))

	// Immediately evaluate positions
	_ = p.EvaluatePositionTriggers()
}
