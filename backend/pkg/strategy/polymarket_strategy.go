package strategy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"tiq/backend/pkg/db"
	"tiq/backend/pkg/engine"
)

type PolymarketConfig struct {
	MarketAddress   string  `json:"market_address"`   // Polymarket target smart contract address
	StrikePrice     float64 `json:"strike_price"`     // Target price (e.g., $71,200)
	ExpirationTime  time.Time `json:"expiration_time"` // Target expiry timestamp
	MinExpectedValue float64 `json:"min_expected_value"` // Minimum EV edge required (e.g., $0.05)
	RiskPercent     float64 `json:"risk_percent"`     // USDC wallet percent to risk per trade
}

type PolymarketRunner struct {
	cfg          PolymarketConfig
	store        *db.DB
	polyEngine   *engine.PolymarketEngine
}

func NewPolymarketRunner(cfg PolymarketConfig, store *db.DB, polyEng *engine.PolymarketEngine) *PolymarketRunner {
	return &PolymarketRunner{
		cfg:        cfg,
		store:      store,
		polyEngine: polyEng,
	}
}

// Tick executes a single 5-minute Polymarket strategy evaluation step
func (pr *PolymarketRunner) Tick(currentPrice float64, atr float64, isBullishTrend bool, marketYesPrice float64, marketNoPrice float64) error {
	timeRemaining := time.Until(pr.cfg.ExpirationTime).Seconds()
	if timeRemaining <= 0 {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Market %s has expired. Skipping tick.", pr.cfg.MarketAddress))
		return nil
	}

	// 1. Position Check: Always monitor open positions first, even outside the entry window
	openShares, err := pr.polyEngine.GetOpenPositions()
	if err != nil {
		return err
	}

	// Extract the unique hex address/condition ID from the market address to prevent duplicate entries on the same contract
	var currentCondID string
	parts := strings.Split(pr.cfg.MarketAddress, "_")
	if len(parts) >= 2 {
		currentCondID = parts[1]
	}

	for _, pos := range openShares {
		posParts := strings.Split(pos.Instrument, "_")
		if len(posParts) >= 2 && posParts[1] == currentCondID {
			// Position exists for this contract! Check exit condition.
			isLong := pos.Units > 0
			var currentSharePrice float64
			if isLong {
				currentSharePrice = marketYesPrice
			} else {
				currentSharePrice = marketNoPrice
			}

			// Update the price of the actual open position's instrument ID (YES price) in the engine feed
			_ = pr.polyEngine.UpdatePrices(map[string]float64{
				pos.Instrument: marketYesPrice,
			})
			// Scalp take-profit: exit once the token reprices up to the stored catch-up target
			if pos.TakeProfit > 0 && currentSharePrice >= pos.TakeProfit {
				pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Take Profit triggered! Share Price: $%.3f >= Target: $%.3f (Entry: $%.3f). Closing position.",
					currentSharePrice, pos.TakeProfit, pos.OpenPrice))
				err = pr.polyEngine.ClosePosition(pos.ID, currentSharePrice)
				return err
			}
			return nil
		}
	}

	// Entry window: from contract open (~300s) down to 150s remaining. This leaves >=60s of
	// runway before the 90s scalp-flatten and stays clear of the near-expiry zone where the
	// probability model saturates. Re-entry within this window is allowed (see below).
	if timeRemaining < 150.0 || timeRemaining > 300.0 {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Time remaining (%.0fs) is outside the entry window (150s to 300s). Skipping trade.", timeRemaining))
		return nil
	}

	// Entry lag window: BTC must have moved enough to signal a real edge, but not so much that
	// Polymarket has already repriced the token past the boundary. $30 minimum ensures real signal;
	// $100 maximum ensures we're still early enough to buy before Polymarket catches up.
	const minLag = 30.0
	const maxLag = 100.0

	lag := math.Abs(currentPrice - pr.cfg.StrikePrice)
	if lag < minLag {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Current lag ($%.2f) below minimum $%.2f. Skipping trade.", lag, minLag))
		return nil
	}
	if lag > maxLag {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Current lag ($%.2f) above maximum $%.2f — Polymarket likely repriced. Skipping trade.", lag, maxLag))
		return nil
	}

	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Evaluating Market %s. Expiry in %.0fs. Current Spot: $%.2f | Lag: $%.2f (min: $%.2f)",
		pr.cfg.MarketAddress, timeRemaining, currentPrice, lag, minLag))

	// 1. Calculate True Probability of resolving YES using the Volatility Engine
	// Volatility per second scaled down from standard 5m ATR
	volatilityPerSec := (atr / currentPrice) / math.Sqrt(300.0)
	if volatilityPerSec <= 0 {
		volatilityPerSec = 0.0001 // Fallback floor
	}

	// Calculate distance to strike (Black-Scholes d1-like probability distance)
	d := math.Log(currentPrice/pr.cfg.StrikePrice) / (volatilityPerSec * math.Sqrt(timeRemaining))
	
	// Cumulative Normal Distribution gives the probability of YES
	trueYesProbability := 0.5 * (1.0 + math.Erf(d/math.Sqrt(2.0)))
	trueNoProbability := 1.0 - trueYesProbability

	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Calculated Probability: YES=%.1f%%, NO=%.1f%% (Realized Volatility: %.4f%%)", 
		trueYesProbability*100, trueNoProbability*100, volatilityPerSec*100))

	// 3. Evaluate Expected Value (EV)
	yesEV := (trueYesProbability * 1.0) - marketYesPrice
	noEV := (trueNoProbability * 1.0) - marketNoPrice

	targetInstrument := pr.cfg.MarketAddress

	// Update contract token prices in the engine price feed so triggers are checked against contract prices
	_ = pr.polyEngine.UpdatePrices(map[string]float64{
		pr.cfg.MarketAddress: marketYesPrice,
		targetInstrument:     marketYesPrice,
	})

	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Market share prices: YES=$%.2f USDC, NO=$%.2f USDC", 
		marketYesPrice, marketNoPrice))
	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Expected Value Edge: YES=+$%.2f USDC, NO=+$%.2f USDC", 
		yesEV, noEV))

	// 4. expected value calculations already completed. Skip check since it was moved to the top.

	// 5. Open Position if Expected Value exceeds our Edge target.
	// Re-entry is intentionally allowed for scalping: the open-position check at the top of
	// Tick already prevents holding two positions on the same contract (no stacking), and the
	// EV-edge + lag gates above self-regulate — we only re-enter once a genuinely fresh
	// mispricing reappears after a prior scalp has closed. (A previous one-trade-per-contract
	// block strangled scalp frequency and has been removed.)
	_, _, err = pr.polyEngine.GetBalance()
	if err != nil {
		pr.store.Log("ERROR", fmt.Sprintf("[Polymarket Strategy] GetBalance failed, skipping trade: %v", err))
		return err
	}

	// Fixed risk of $5 USDC per trade as requested by the user
	riskCapital := 5.00

	if yesEV >= pr.cfg.MinExpectedValue {
		if err := pr.enterLiveEdge(targetInstrument, true, trueYesProbability, marketYesPrice, riskCapital); err != nil {
			return err
		}
	} else if noEV >= pr.cfg.MinExpectedValue {
		if err := pr.enterLiveEdge(targetInstrument, false, trueNoProbability, marketNoPrice, riskCapital); err != nil {
			return err
		}
	} else {
		pr.store.Log("INFO", "[Polymarket Strategy] Expected Value edge insufficient. HOLD/Wait.")
	}

	return nil
}

// enterLiveEdge re-evaluates a detected edge against the LIVE CLOB order book
// before committing. The feed-derived EV that got us here uses a lagged
// last-trade price; the real executable price can be well above it once
// Polymarket has repriced. We fetch the marketable ask for our size and:
//   - skip if the book is too thin to fill,
//   - skip if the edge no longer clears the threshold at the real price
//     (the lag already closed — filling here would overpay),
//   - otherwise buy AT the marketable price so the FOK actually crosses.
//
// isYes selects the token; feedPrice is the stale price used only for logging
// and initial sizing; trueProb is the Black-Scholes fair probability.
func (pr *PolymarketRunner) enterLiveEdge(targetInstrument string, isYes bool, trueProb, feedPrice, riskCapital float64) error {
	side := "NO"
	sign := -1.0
	if isYes {
		side, sign = "YES", 1.0
	}

	estUnits := riskCapital / feedPrice
	livePrice, fillable, err := pr.polyEngine.GetMarketablePrice(isYes, 0, estUnits)
	if err != nil || !fillable {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s edge seen but live book unavailable/too thin (fillable=%t, err=%v) — skipping.", side, fillable, err))
		return nil
	}

	liveEdge := trueProb - livePrice
	if liveEdge < pr.cfg.MinExpectedValue {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s edge gone at LIVE ask $%.2f (feed showed $%.2f, live edge $%.2f < $%.2f) — Polymarket already repriced. Skipping.",
			side, livePrice, feedPrice, liveEdge, pr.cfg.MinExpectedValue))
		return nil
	}

	// Boundary guard against the real price, not the stale feed.
	if livePrice < 0.35 || livePrice > 0.65 {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Live %s ask $%.2f outside $0.35–$0.65 boundary — skipping.", side, livePrice))
		return nil
	}

	units := sign * riskCapital / livePrice
	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] LIVE edge confirmed! Buying %.2f %s @ live ask $%.2f (edge $%.2f; feed said $%.2f). Risk $%.2f.",
		math.Abs(units), side, livePrice, liveEdge, feedPrice, riskCapital))

	// Scalp limits anchored to the real fill price: SL = entry-$0.03, TP = entry + 60% of live edge.
	_, err = pr.polyEngine.OpenPosition(targetInstrument, units, livePrice, livePrice-0.03, livePrice+0.6*liveEdge)
	return err
}

