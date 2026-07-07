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
	if volatilityPerSec < minVolPerSec {
		// Floor prevents the Black-Scholes model from saturating to ~92% on a tiny
		// spot-vs-strike gap when realized vol reads near zero. A near-zero vol made
		// the model near-certain on a coin-flip strike and produced a fake $0.5 edge.
		volatilityPerSec = minVolPerSec
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

// Entry guardrails, added after a live loss where a mis-calibrated model bought a
// collapsing token at a stale, wide-book ask ($0.65 for a token really worth
// $0.25) and lost 15x the intended stop. Each gate below independently blocks
// that trade. Values are tunable.
const (
	maxEntryEdge  = 0.20   // edge ceiling: a real latency lag is a few cents; a
	                       // larger model-vs-market gap means our model is wrong.
	maxBookSpread = 0.04   // reject wide/illiquid books (bestAsk-bestBid): buying
	                       // the ask there marks us instantly at a far-lower bid.
	maxTradeProb  = 0.80   // reject saturated model probabilities (near-certain
	                       // reads on coin-flip strikes with ~zero vol).
	minVolPerSec  = 0.0002 // volatility floor (see Tick): stops model saturation.
)

// enterLiveEdge re-evaluates a detected edge against the LIVE CLOB order book and
// applies the entry guardrails before committing real capital. The feed-derived
// EV that got us here uses a lagged, noisy last-trade price; we re-check against
// the real book and refuse the trade unless it is genuinely a small, tradeable
// latency lag on a tight two-sided book.
//
// isYes selects the token; feedPrice is the stale price (logging/initial sizing);
// trueProb is the Black-Scholes fair probability.
func (pr *PolymarketRunner) enterLiveEdge(targetInstrument string, isYes bool, trueProb, feedPrice, riskCapital float64) error {
	side := "NO"
	sign := -1.0
	if isYes {
		side, sign = "YES", 1.0
	}

	// GATE (probability band): a saturated model probability near a coin-flip
	// strike is exactly the untrustworthy kind — refuse it.
	if trueProb > maxTradeProb {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s model prob %.0f%% saturated (> %.0f%%) — untrustworthy near-certain signal, skipping.", side, trueProb*100, maxTradeProb*100))
		return nil
	}

	// GATE (probability band): a saturated model probability near a coin-flip
	// strike is exactly the untrustworthy kind — refuse it before any book I/O.
	if trueProb > maxTradeProb {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s model prob %.0f%% saturated (> %.0f%%) — untrustworthy near-certain signal, skipping.", side, trueProb*100, maxTradeProb*100))
		return nil
	}

	estUnits := riskCapital / feedPrice
	bestBid, bestAsk, buyPrice, ok, err := pr.polyEngine.EvaluateEntryBook(isYes, estUnits)
	if err != nil || !ok {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s edge seen but book one-sided/thin/unavailable (ok=%t, err=%v) — skipping.", side, ok, err))
		return nil
	}

	proceed, liveEdge, reason := checkEntryGates(trueProb, bestBid, bestAsk, buyPrice, pr.cfg.MinExpectedValue)
	if !proceed {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s entry blocked by guardrail: %s (bid $%.2f / ask $%.2f, buy $%.2f, feed $%.2f).",
			side, reason, bestBid, bestAsk, buyPrice, feedPrice))
		return nil
	}

	units := sign * riskCapital / buyPrice
	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] LIVE edge confirmed! Buying %.2f %s @ $%.2f (edge $%.2f; bid $%.2f / ask $%.2f; feed $%.2f). Risk $%.2f.",
		math.Abs(units), side, buyPrice, liveEdge, bestBid, bestAsk, feedPrice, riskCapital))

	// Scalp limits anchored to the real fill price: SL = entry-$0.03, TP = entry + 60% of live edge.
	_, err = pr.polyEngine.OpenPosition(targetInstrument, units, buyPrice, buyPrice-0.03, buyPrice+0.6*liveEdge)
	return err
}

// checkEntryGates applies the pure entry guardrails (no I/O) and reports whether a
// trade may proceed, the edge at the real executable price, and — when blocked —
// the reason. Separated from enterLiveEdge so the thresholds are unit-testable.
// The probability-band gate is applied by the caller before book I/O.
func checkEntryGates(trueProb, bestBid, bestAsk, buyPrice, minEdge float64) (proceed bool, edge float64, reason string) {
	// Book sanity: a wide spread is an illiquid/unstable book; buying the ask
	// marks us instantly at the far-lower bid.
	if spread := bestAsk - bestBid; spread > maxBookSpread {
		return false, 0, fmt.Sprintf("book too wide (spread $%.2f > $%.2f)", spread, maxBookSpread)
	}
	edge = trueProb - buyPrice
	// Edge floor: the lag must still exist at the real price.
	if edge < minEdge {
		return false, edge, fmt.Sprintf("edge $%.2f below min $%.2f — repriced", edge, minEdge)
	}
	// Edge ceiling: an outsized edge is a model-vs-market disagreement (bad
	// vol/strike), not a latency lag. Strongest block on the loss trade.
	if edge > maxEntryEdge {
		return false, edge, fmt.Sprintf("edge $%.2f exceeds max $%.2f — implausible model/market gap", edge, maxEntryEdge)
	}
	// Boundary on the real price.
	if buyPrice < 0.35 || buyPrice > 0.65 {
		return false, edge, fmt.Sprintf("price $%.2f outside $0.35–$0.65 boundary", buyPrice)
	}
	return true, edge, ""
}

