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
	MarketAddress    string    `json:"market_address"`     // Polymarket target smart contract address
	StrikePrice      float64   `json:"strike_price"`       // Target price (e.g., $71,200)
	ExpirationTime   time.Time `json:"expiration_time"`    // Target expiry timestamp
	MinExpectedValue float64   `json:"min_expected_value"` // Minimum EV edge required (e.g., $0.05)
	RiskPercent      float64   `json:"risk_percent"`       // USDC wallet percent to risk per trade
}

type PolymarketRunner struct {
	cfg        PolymarketConfig
	store      *db.DB
	polyEngine *engine.PolymarketEngine
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

	// Structured book snapshot for offline persistence/momentum analysis. Logs the REAL
	// top-of-book (not the noisy last-trade tape) once per tick, tagged by condition ID
	// so a future check can build a clean same-contract time series directly via SQL
	// filtering on "cond=<id>", instead of reconstructing it from ambiguous trade-print
	// logs after the fact (which is why our first attempt at this analysis was
	// inconclusive — most trades had too few usable, unambiguously-same-contract points).
	//
	// Also logs spot/strike/dist (dist = spot-strike; positive favors YES) for the
	// S2/S3 late-window lag-strategy calibration: the strategy's own "Current Spot"
	// log line is gated inside the 150-300s entry-window check and NEVER fires below
	// t_remain=150s (confirmed empirically — zero exceptions across 2000+ log lines),
	// but S2 operates at t_remain<60s, so there was no historical data to calibrate
	// against. This line already runs every tick regardless of window, so it's the
	// natural place to close that gap going forward.
	if yBid, yAsk, ok1 := pr.polyEngine.GetTopOfBook(true); ok1 {
		if nBid, nAsk, ok2 := pr.polyEngine.GetTopOfBook(false); ok2 {
			pr.store.Log("INFO", fmt.Sprintf("[BookSnapshot] cond=%s t_remain=%.0f yes_bid=%.3f yes_ask=%.3f no_bid=%.3f no_ask=%.3f spot=%.2f strike=%.2f dist=%.2f",
				currentCondID, timeRemaining, yBid, yAsk, nBid, nAsk, currentPrice, pr.cfg.StrikePrice, currentPrice-pr.cfg.StrikePrice))
		}
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

	// S2: late-window decisive-lag strategy. Operates in the closing seconds, well
	// outside (and mutually exclusive with) the normal 150-300s entry window below —
	// no existing position was found on this contract above, so it's safe to check.
	if timeRemaining <= s2MaxTimeRemaining {
		return pr.tryS2Entry(currentPrice, atr, timeRemaining, marketYesPrice, marketNoPrice)
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
	maxEntryEdge = 0.20 // edge ceiling: a real latency lag is a few cents; a
	// larger model-vs-market gap means our model is wrong.
	maxBookSpread = 0.04 // reject wide/illiquid books (bestAsk-bestBid): buying
	// the ask there marks us instantly at a far-lower bid.
	maxTradeProb = 0.80 // reject saturated model probabilities (near-certain
	// reads on coin-flip strikes with ~zero vol).
	minVolPerSec = 0.0002 // volatility floor (see Tick): stops model saturation.
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

	// GATE (re-entry cooldown): refuse to re-enter this contract right after a
	// stop-loss closed a position on it. A stop-out often means the underlying
	// move is still running; re-entering immediately just buys back into it.
	// Checked first — cheapest gate, no book I/O.
	if remaining, active := pr.polyEngine.InCooldown(targetInstrument); active {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s entry blocked: contract in re-entry cooldown (%.0fs remaining after a recent stop-loss).", side, remaining.Seconds()))
		return nil
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

// S2: late-window "decisive Kraken lag" strategy. Distinct from the main
// entry logic above — it doesn't bet on a model-vs-market EDGE, it waits until
// spot is decisively past the strike in the closing seconds and buys the side
// Kraken already favors, holding to free settlement redemption instead of a
// normal SL/TP exit (see ResolvePosition and the reversal bail-out in
// EvaluatePositionTriggers).
//
// These thresholds are an explicit, UNCALIBRATED starting hypothesis, not a
// fitted number — we confirmed there is zero historical spot-price data below
// t_remain=150s to calibrate against (the existing "Current Spot" log line
// is gated inside the normal entry-window check and never fires below it).
// The extended BookSnapshot logging (spot/strike/dist, every tick, any
// t_remain) exists specifically to accumulate the data needed to calibrate
// these numbers properly later.
const (
	s2MaxTimeRemaining = 60.0 // only operate in the closing seconds
	s2MinProb          = 0.90 // Black-Scholes-style model confidence floor
	// Sanity floor independent of the volatility-scaled probability above.
	// Guards against the exact overconfidence failure minVolPerSec exists to
	// prevent elsewhere: near-zero measured volatility can make the model
	// read 99% confident on a real gap that's actually quite small.
	s2MinAbsDistance = 50.0
	// Leaves margin over the ~$0.925 fee-adjusted breakeven (taker fee at
	// $0.93 is cheap, but buying at $0.99 leaves ~nothing to gain).
	s2MaxEntryPrice = 0.93
	// Fixed risk size, matching the main strategy's per-trade sizing.
	s2RiskCapital = 5.00
)

// checkS2Gates applies S2's pure entry conditions (no I/O), mirroring
// checkEntryGates' separation so the thresholds are unit-testable independent
// of book/network calls.
func checkS2Gates(dist, prob, buyPrice float64) (proceed bool, reason string) {
	if math.Abs(dist) < s2MinAbsDistance {
		return false, fmt.Sprintf("spot-strike distance $%.0f below $%.0f floor — too close to trust this late", dist, s2MinAbsDistance)
	}
	if prob < s2MinProb {
		return false, fmt.Sprintf("model confidence %.1f%% below %.0f%% floor", prob*100, s2MinProb*100)
	}
	if buyPrice > s2MaxEntryPrice {
		return false, fmt.Sprintf("book already at $%.2f — no margin left over $%.2f max", buyPrice, s2MaxEntryPrice)
	}
	return true, ""
}

// tryS2Entry checks S2's trigger and, if it fires, opens a single-sided,
// hold-to-redemption position (instrument tagged "_s2"). Only reached when no
// position already exists on this contract (checked by the caller) and
// timeRemaining <= s2MaxTimeRemaining.
func (pr *PolymarketRunner) tryS2Entry(currentPrice, atr, timeRemaining, marketYesPrice, marketNoPrice float64) error {
	volatilityPerSec := (atr / currentPrice) / math.Sqrt(300.0)
	if volatilityPerSec < minVolPerSec {
		volatilityPerSec = minVolPerSec
	}
	d := math.Log(currentPrice/pr.cfg.StrikePrice) / (volatilityPerSec * math.Sqrt(timeRemaining))
	trueYesProbability := 0.5 * (1.0 + math.Erf(d/math.Sqrt(2.0)))
	trueNoProbability := 1.0 - trueYesProbability

	dist := currentPrice - pr.cfg.StrikePrice
	isYes := dist > 0
	side, prob, feedPrice := "NO", trueNoProbability, marketNoPrice
	if isYes {
		side, prob, feedPrice = "YES", trueYesProbability, marketYesPrice
	}

	if feedPrice <= 0 {
		return nil
	}
	estUnits := s2RiskCapital / feedPrice
	bestBid, bestAsk, buyPrice, ok, err := pr.polyEngine.EvaluateEntryBook(isYes, estUnits)
	if err != nil || !ok {
		return nil // book one-sided/thin/unavailable — silent, this is the common case far from the trigger
	}

	proceed, reason := checkS2Gates(dist, prob, buyPrice)
	if !proceed {
		// Only log the "decisive but priced out" case — useful for tuning
		// s2MaxEntryPrice. The distance/probability misses are the overwhelming
		// majority of ticks and would just be noise.
		if math.Abs(dist) >= s2MinAbsDistance && prob >= s2MinProb {
			pr.store.Log("INFO", fmt.Sprintf("[S2] %s decisive (prob %.1f%%, dist $%.0f, t_remain=%.0fs) but %s", side, prob*100, dist, timeRemaining, reason))
		}
		return nil
	}

	units := s2RiskCapital / buyPrice
	if !isYes {
		units = -units
	}
	pr.store.Log("INFO", fmt.Sprintf("[S2] Decisive late-window entry! Buying %.2f %s @ $%.2f (bid $%.2f/ask $%.2f, model prob %.1f%%, spot-strike dist $%.0f, t_remain=%.0fs). Holding to redemption.",
		math.Abs(units), side, buyPrice, bestBid, bestAsk, prob*100, dist, timeRemaining))

	marketAddr := pr.cfg.MarketAddress + "_s2"
	_, err = pr.polyEngine.OpenPosition(marketAddr, units, buyPrice, 0, 0)
	return err
}
