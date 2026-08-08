package strategy

import (
	"fmt"
	"math"
	"strings"
	"sync"
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

// s1Variants defines the parallel A/B/C threshold experiment: three otherwise-
// identical S1 strategies running simultaneously on every contract, differing
// only in the edge floor. Positions are tagged with the variant tag (instrument
// suffix "_v05"/"_v08"/"_v12") so win rate and PnL can be compared per variant.
// The variants are independent — separate per-contract entry dedup and separate
// re-entry cooldowns (see cooldownKey) — so one variant's activity can't
// contaminate another's results. They share the same book, price feed, and the
// single S2 late-window strategy, which run once per tick regardless.
var s1Variants = []struct {
	tag     string
	minEdge float64
}{
	{"v05", 0.05},
	{"v08", 0.08},
	{"v12", 0.12},
}

// hasOpenForVariant reports whether an open position already exists for this
// contract (condID) tagged with the given variant/strategy suffix (e.g. "_v08",
// "_s2"). Used for per-variant entry dedup so each variant holds at most one
// position per contract at a time.
func hasOpenForVariant(openShares []db.Position, condID, suffix string) bool {
	for _, pos := range openShares {
		pp := strings.Split(pos.Instrument, "_")
		if len(pp) >= 2 && pp[1] == condID && strings.HasSuffix(pos.Instrument, suffix) {
			return true
		}
	}
	return false
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

	// Keep the price feed fresh for any open position on this contract. All
	// variants + S2 buy the same YES/NO token (priced by condition ID), so this
	// one update covers every open position. Exits (TP/SL/flatten) are handled
	// by the engine's book-based EvaluatePositionTriggers loop, NOT here — the
	// old tape-priced TP that lived here fired on last-trade noise and was
	// superseded by the book-based trigger.
	for _, pos := range openShares {
		posParts := strings.Split(pos.Instrument, "_")
		if len(posParts) >= 2 && posParts[1] == currentCondID {
			_ = pr.polyEngine.UpdatePrices(map[string]float64{pos.Instrument: marketYesPrice})
		}
	}

	// S3: two-sided dislocation arb. Checked EVERY tick at any point in the
	// window (measured events spread evenly across the window) — buys BOTH
	// tokens when the executable pair cost locks >= s3MinNetPerPair after fees,
	// then holds to free settlement redemption (exactly one leg pays $1).
	// Direction-irrelevant by construction; skipped while a pair is open.
	if pr.polyEngine.StrategyEnabled("s3") && timeRemaining > 5 &&
		!hasOpenForVariant(openShares, currentCondID, "_s3y") &&
		!hasOpenForVariant(openShares, currentCondID, "_s3n") {
		if err := pr.tryS3Entry(currentCondID); err != nil {
			return err
		}
	}

	// S2: late-window decisive-lag strategy. Runs ONCE per tick (not per S1
	// variant), only if no S2 position is already open on this contract.
	// Operates in the closing seconds, mutually exclusive with the 150-300s S1
	// entry window below. S4's window (<=30s) is a subset of this one, so it's
	// evaluated here too instead of behind an early return — otherwise S4 would
	// never get a chance to run.
	if timeRemaining <= s2MaxTimeRemaining {
		if pr.polyEngine.StrategyEnabled("s2") && !hasOpenForVariant(openShares, currentCondID, "_s2") {
			if err := pr.tryS2Entry(currentPrice, atr, timeRemaining, marketYesPrice, marketNoPrice); err != nil {
				return err
			}
		}
		if timeRemaining <= s4MaxTimeRemaining {
			if pr.polyEngine.StrategyEnabled("s4") && !hasOpenForVariant(openShares, currentCondID, "_s4") {
				return pr.tryS4Entry(currentPrice, timeRemaining)
			}
		}
		return nil
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

	// Fixed dollar risk per trade. $4 (down from $5) to stretch an ~$11.7 live
	// balance while keeping a healthy safety margin. Because we size by dollars,
	// $4 stays above Polymarket's 5-share orderMinSize up to an entry price of
	// $0.80 ($4/$0.80 = 5 shares); above that the order rounds below 5 shares and
	// the CLOB rejects it — but every real entry so far has been <= $0.51, so
	// there's ample margin. Note: $4 only funds ~2 concurrent open positions on
	// this balance, which is fine since scalps almost never overlap. S2 keeps its
	// own s2RiskCapital=$5 (it buys up to $0.93, needing >=$4.65 to clear 5 shares).
	riskCapital := 4.00

	// A/B/C threshold experiment: evaluate each variant independently. A variant
	// enters (buying the tagged instrument "<market>_<tag>") only if it has no
	// open position on this contract and the EV clears ITS edge floor. Because
	// the floors are nested (0.05 ⊂ 0.08 ⊂ 0.12), a high-edge setup enters all
	// three; a marginal one enters only the looser variants — which is exactly
	// the comparison we want. targetInstrument (the base, no suffix) is retained
	// only for the price-feed update above.
	if !pr.polyEngine.StrategyEnabled("s1") {
		return nil // live mode with S1 not allowlisted — S1 stays parked
	}
	anyEntered := false
	for _, v := range s1Variants {
		suffix := "_" + v.tag
		if hasOpenForVariant(openShares, currentCondID, suffix) {
			continue // this variant already holds a position; engine manages its exit
		}
		variantInstrument := pr.cfg.MarketAddress + suffix
		if yesEV >= v.minEdge {
			anyEntered = true
			if err := pr.enterLiveEdge(variantInstrument, true, trueYesProbability, marketYesPrice, riskCapital, v.minEdge); err != nil {
				return err
			}
		} else if noEV >= v.minEdge {
			anyEntered = true
			if err := pr.enterLiveEdge(variantInstrument, false, trueNoProbability, marketNoPrice, riskCapital, v.minEdge); err != nil {
				return err
			}
		}
	}
	if !anyEntered {
		pr.store.Log("INFO", "[Polymarket Strategy] Expected Value edge below all variant floors (0.05/0.08/0.12). HOLD/Wait.")
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
func (pr *PolymarketRunner) enterLiveEdge(targetInstrument string, isYes bool, trueProb, feedPrice, riskCapital, minEdge float64) error {
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

	proceed, liveEdge, reason := checkEntryGates(trueProb, bestBid, bestAsk, buyPrice, minEdge)
	if !proceed {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] %s (floor $%.2f) entry blocked by guardrail: %s (bid $%.2f / ask $%.2f, buy $%.2f, feed $%.2f).",
			side, minEdge, reason, bestBid, bestAsk, buyPrice, feedPrice))
		return nil
	}

	units := sign * riskCapital / buyPrice
	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] LIVE edge confirmed [floor $%.2f -> %s]! Buying %.2f %s @ $%.2f (edge $%.2f; bid $%.2f / ask $%.2f; feed $%.2f). Risk $%.2f.",
		minEdge, targetInstrument, math.Abs(units), side, buyPrice, liveEdge, bestBid, bestAsk, feedPrice, riskCapital))

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
// S2 — the "buffer strategy", re-calibrated from a clean historical
// reconstruction (loganalysis). Buy the decisive favorite in the 1-2 minute
// window and hold to FREE redemption. What the reconstruction found, at
// t_remain 60-120s:
//   - BTC $30-100 clear of strike -> the ~$0.90 favorite won ~95%
//   - BTC within $30 -> only ~70% (the coin-flip TRAP; excluded)
//
// ~95% clears the ~91.6% fee-adjusted breakeven with margin — IF the win rate
// holds. The confirming sample is small (21-37), so this is a PAPER forward-test
// to bank 100+ real trades before any live use. Gates are buffer/price-based
// (what was validated), not the old Black-Scholes-probability gate.
const (
	s2MaxTimeRemaining = 120.0 // enter within the last 2 minutes...
	s2MinTimeRemaining = 45.0  // ...but not so late there's no room to bail
	s2MinAbsDistance   = 30.0  // BTC must be >= $30 clear of strike (skip the <30 trap)
	s2MinEntryPrice    = 0.85  // favorite must be priced as a real favorite...
	s2MaxEntryPrice    = 0.95  // ...up to $0.95, to also catch the decisive $38+ buffer setups
	//                            the market prices at $0.93-0.95 (breakeven ~91% up there, so
	//                            these lean on the higher win rate a bigger buffer implies).
	s2RiskCapital = 2.00 // $2/trade: caps a typical bailed loss under $1 and sizes
	//                       sanely for an ~$11.75 balance (was $5). Wins shrink to
	//                       ~$0.17 in step; the win/loss ratio is unchanged.
)

// checkS2Gates applies S2's pure entry conditions (no I/O), mirroring
// checkEntryGates' separation so the thresholds are unit-testable independent
// of book/network calls.
func checkS2Gates(dist, buyPrice float64) (proceed bool, reason string) {
	if math.Abs(dist) < s2MinAbsDistance {
		return false, fmt.Sprintf("buffer $%.0f below $%.0f floor — inside the coin-flip zone", math.Abs(dist), s2MinAbsDistance)
	}
	if buyPrice < s2MinEntryPrice {
		return false, fmt.Sprintf("favorite only $%.2f (< $%.2f) — market doesn't agree it's decisive", buyPrice, s2MinEntryPrice)
	}
	if buyPrice > s2MaxEntryPrice {
		return false, fmt.Sprintf("book already at $%.2f — no margin over $%.2f to the $1 redemption", buyPrice, s2MaxEntryPrice)
	}
	return true, ""
}

// tryS2Entry checks S2's trigger and, if it fires, opens a single-sided,
// hold-to-redemption position (instrument tagged "_s2"). Only reached when no
// position already exists on this contract (checked by the caller) and
// timeRemaining <= s2MaxTimeRemaining.
func (pr *PolymarketRunner) tryS2Entry(currentPrice, atr, timeRemaining, marketYesPrice, marketNoPrice float64) error {
	// Enter only in the validated 45-120s window — early enough that the $0.50
	// price stop still has room to work, late enough to be "decided".
	if timeRemaining < s2MinTimeRemaining {
		return nil
	}

	// Buffer = how far BTC is (in $) from the strike, in the favored direction.
	dist := currentPrice - pr.cfg.StrikePrice
	isYes := dist > 0
	side, feedPrice := "NO", marketNoPrice
	if isYes {
		side, feedPrice = "YES", marketYesPrice
	}
	if feedPrice <= 0 {
		return nil
	}

	estUnits := s2RiskCapital / feedPrice
	bestBid, bestAsk, buyPrice, ok, err := pr.polyEngine.EvaluateEntryBook(isYes, estUnits)
	if err != nil || !ok {
		return nil // book one-sided/thin/unavailable — silent, common far from the trigger
	}

	proceed, reason := checkS2Gates(dist, buyPrice)
	if !proceed {
		// Log only the "decisive buffer but priced out" case (useful for tuning
		// the price band); the buffer misses are the overwhelming majority.
		if math.Abs(dist) >= s2MinAbsDistance {
			pr.store.Log("INFO", fmt.Sprintf("[S2] %s buffer $%.0f decisive (t_remain=%.0fs, buy $%.2f) but %s", side, math.Abs(dist), timeRemaining, buyPrice, reason))
		}
		return nil
	}

	units := s2RiskCapital / buyPrice
	if !isYes {
		units = -units
	}
	pr.store.Log("INFO", fmt.Sprintf("[S2] Buffer entry! Buying %.2f %s @ $%.2f (bid $%.2f/ask $%.2f, BTC buffer $%.0f, t_remain=%.0fs). Hold to redemption; $0.50 price-stop.",
		math.Abs(units), side, buyPrice, bestBid, bestAsk, math.Abs(dist), timeRemaining))

	marketAddr := pr.cfg.MarketAddress + "_s2"
	_, err = pr.polyEngine.OpenPosition(marketAddr, units, buyPrice, 0, 0)
	return err
}

// S4: "buy the final-30s favorite" — user's hypothesis, distinct from S2. No
// buffer floor, no price band: whichever side (YES/NO) is currently priced
// higher gets bought, at whatever price that is, in the last 30s, held blind
// to free redemption. No bail-out (per spec: "hold to redemption") — this is
// deliberately the rawest form of the idea so paper mode measures exactly what
// was proposed, not a hedged version of it.
//
// A historical backtest (backend/cmd/s4backtest, 2008 real contracts over 7
// days) found this loses money at every price level tested: realized win
// rate sits at or just below each band's own fee-adjusted breakeven (e.g.
// $0.60-0.70 band: 65.9% win vs 66.5% breakeven; $0.80-0.90: 84.4% vs 86.1%).
// That backtest priced entries at the traded/mid price; a real FOK buy pays
// the ask (1-2c worse), so live fills should be worse still.
//
// A 20-trade paper run against real live asks came out +$6.38 (19W/1 exact
// breakeven), contradicting the backtest. That sample is far too small to
// settle the disagreement — at these thin per-trade edges a 19/20 run is well
// within variance — but it was enough for the account owner to take S4 live.
// The backtest's negative result therefore remains UNREFUTED, not disproven.
//
// Taken live 2026-08-02, then shut down the same night: 11 trades, 8W/3L, net
// ~-$7. All 3 losses shared one signature the 8 wins never had — bought at a
// near coin-flip book price ($0.54-$0.70) AND our own BTC spot feed disagreed
// with the book's favorite. Both conditions below are gates added directly in
// response to that: neither existed in the version that went live and lost.
const (
	s4MaxTimeRemaining = 30.0 // enter only in the final 30s, per spec
	// Fixed SHARE count, not a fixed dollar stake (S2 sizes by dollars). 5 shares
	// is also exactly Polymarket's orderMinSize for these markets, so no entry can
	// round below the exchange minimum regardless of price.
	//
	// NOTE this makes per-trade exposure price-scaled instead of flat: cost =
	// 5 * price, so a $0.97 favorite risks ~$4.85 while a $0.30 one risks ~$1.50.
	// Near $1.00 that is ~2.5x the old flat-$2 exposure, and S4 has NO stop-loss
	// (it holds blind to redemption), so a losing high-price entry forfeits the
	// full ~$5.
	s4FixedShares = 5.0

	// GATE (min price): reject coin-flip "favorites". All 3 live losses bought
	// at $0.54-$0.70; every live win bought at $0.76+. $0.85 sits with clear
	// margin above the entire loss cluster — deliberately above the cheapest
	// historical win ($0.76) too, trading a few thin wins for a hard floor
	// under every observed loss rather than threading between them.
	s4MinEntryPrice = 0.85

	// GATE (re-entry guard): at most one entry ATTEMPT per contract. A failed
	// FOK previously left the contract eligible to retry seconds later on a
	// still-moving book — observed live as 2-3 attempts on the same contract,
	// each closer to expiry and further into the price move. 35s outlives the
	// entire 30s entry window, so by the time it would expire the contract has
	// already settled and the next one has a different MarketAddress anyway —
	// this is really "one shot per contract," the duration just needs to clear
	// the window.
	s4RetryCooldown = 35 * time.Second

	// GATE (sweep depth): reject if filling all 5 shares requires dipping more
	// than 3c below the best ask. A wide gap between the top-of-book quote and
	// the price actually needed to fill our size is a direct signal the book is
	// thin/unstable RIGHT NOW — a case the min-price floor alone wouldn't
	// reliably catch (a book quoting $0.97 at the top can still swept-price at
	// $0.86 for 5 shares on thin depth, clearing s4MinEntryPrice while being a
	// genuinely bad book to trade into). Observed live top-of-book spreads were
	// consistently ~$0.01; 3c is well outside normal spread noise.
	s4MaxSweepDepth = 0.03
)

var (
	s4LastAttemptMu sync.Mutex
	s4LastAttempt   = map[string]time.Time{} // MarketAddress -> last entry attempt
)

// checkS4SpotAgreement is the pure spot-agreement gate (see tryS4Entry),
// separated out so it's unit-testable independent of the book/network calls.
func checkS4SpotAgreement(isYes, spotFavorsYes bool) (proceed bool, reason string) {
	if isYes != spotFavorsYes {
		return false, "book favorite disagrees with our spot feed"
	}
	return true, ""
}

// checkS4Price is the pure minimum-price gate (see tryS4Entry).
func checkS4Price(buyPrice float64) (proceed bool, reason string) {
	if buyPrice < s4MinEntryPrice {
		return false, fmt.Sprintf("price $%.2f below $%.2f floor — not a real favorite", buyPrice, s4MinEntryPrice)
	}
	return true, ""
}

// checkS4SweepDepth is the pure sweep-depth gate (see tryS4Entry): how far the
// price needed to fill the full size sits above the best ask (a BUY sweep
// only ever gets more expensive with depth, never cheaper).
func checkS4SweepDepth(bestAsk, buyPrice float64) (proceed bool, reason string) {
	depth := buyPrice - bestAsk
	// Epsilon guards the boundary against float noise: real inputs are always
	// cent-rounded (EvaluateEntryBook ceils to the $0.01 tick), and comparing
	// two such values can land a hair on either side of an exact-looking cap.
	if depth > s4MaxSweepDepth+1e-9 {
		return false, fmt.Sprintf("book too thin: filling %.0f shares needs $%.2f (best ask $%.2f, depth $%.2f > $%.2f cap)", s4FixedShares, buyPrice, bestAsk, depth, s4MaxSweepDepth)
	}
	return true, ""
}

// tryS4Entry buys whichever side is currently priced higher, gated by
// checkS4SpotAgreement, checkS4SweepDepth and checkS4Price above plus a
// per-contract re-entry guard. Only reached when no S4 position already
// exists on this contract (checked by the caller) and
// timeRemaining <= s4MaxTimeRemaining.
//
// Side selection reads the LIVE BOOK (GetTopOfBook), never the last-trade
// tape — a stale tape print can disagree sharply with where the book
// actually sits, especially inside the final 30s where prices move fast. A
// real paper trade caught this the hard way: the tape said one side was
// ahead, but the book had it priced at $0.02 (the actual dead underdog) —
// this function bought whichever side the tape claimed, never re-checking it
// against the book it then fetched to price the fill. The rest of this
// codebase already learned this lesson (see the tape-vs-book note in
// EvaluatePositionTriggers); S4 didn't have it. Fixed by deriving both the
// side AND the fill price from one live book read.
func (pr *PolymarketRunner) tryS4Entry(currentPrice, timeRemaining float64) error {
	// GATE (re-entry guard): at most one attempt per contract, checked first —
	// cheapest gate, no book I/O.
	s4LastAttemptMu.Lock()
	if t, seen := s4LastAttempt[pr.cfg.MarketAddress]; seen && time.Since(t) < s4RetryCooldown {
		s4LastAttemptMu.Unlock()
		return nil
	}
	s4LastAttemptMu.Unlock()

	yBid, yAsk, okY := pr.polyEngine.GetTopOfBook(true)
	nBid, nAsk, okN := pr.polyEngine.GetTopOfBook(false)
	if !okY || !okN {
		return nil // one-sided/unavailable book — can't compare sides, skip
	}
	yMid := (yBid + yAsk) / 2
	nMid := (nBid + nAsk) / 2
	isYes := yMid >= nMid
	side := "NO"
	if isYes {
		side = "YES"
	}

	// GATE (spot agreement): refuse to buy the book's favorite unless our own
	// BTC spot feed agrees it's actually ahead. On the live data that produced
	// the losses this fixes, it's a perfect separator: all 8 wins had spot and
	// book agreeing, all 3 losses had them disagreeing (book still favored the
	// old side while spot had already crossed to the other one).
	spotFavorsYes := currentPrice >= pr.cfg.StrikePrice
	if proceed, reason := checkS4SpotAgreement(isYes, spotFavorsYes); !proceed {
		pr.store.Log("INFO", fmt.Sprintf("[S4] %s: %s (spot $%.2f vs strike $%.2f) — skipping, t_remain=%.0fs.",
			side, reason, currentPrice, pr.cfg.StrikePrice, timeRemaining))
		return nil
	}

	// Size is a fixed share count, so the depth check asks for exactly what we
	// intend to buy — no estimate/actual mismatch as with dollar-based sizing.
	bestBid, bestAsk, buyPrice, ok, err := pr.polyEngine.EvaluateEntryBook(isYes, s4FixedShares)
	if err != nil || !ok {
		return nil // book one-sided/thin/unavailable — silent, same as S2/S3
	}

	// GATE (sweep depth): the book must have real depth at the top, not just a
	// tempting quote that thins out immediately below it.
	if proceed, reason := checkS4SweepDepth(bestAsk, buyPrice); !proceed {
		pr.store.Log("INFO", fmt.Sprintf("[S4] %s: %s. t_remain=%.0fs.", side, reason, timeRemaining))
		return nil
	}

	// GATE (min price): the book must show real conviction, not a coin flip.
	if proceed, reason := checkS4Price(buyPrice); !proceed {
		pr.store.Log("INFO", fmt.Sprintf("[S4] %s: %s. t_remain=%.0fs.", side, reason, timeRemaining))
		return nil
	}

	// Past all gates — this is a real attempt. Mark it before submitting so a
	// slow/failing order can't retry within the same contract's window.
	s4LastAttemptMu.Lock()
	s4LastAttempt[pr.cfg.MarketAddress] = time.Now()
	if len(s4LastAttempt) > 50 { // bound: contracts roll every 5 minutes forever
		for k, v := range s4LastAttempt {
			if time.Since(v) > time.Hour {
				delete(s4LastAttempt, k)
			}
		}
	}
	s4LastAttemptMu.Unlock()

	units := s4FixedShares
	if !isYes {
		units = -units
	}
	pr.store.Log("INFO", fmt.Sprintf("[S4] Final-30s entry! Buying %.0f %s @ $%.2f = $%.2f (bid $%.2f/ask $%.2f, spot $%.2f vs strike $%.2f, t_remain=%.0fs). Hold to redemption.",
		math.Abs(units), side, buyPrice, math.Abs(units)*buyPrice, bestBid, bestAsk, currentPrice, pr.cfg.StrikePrice, timeRemaining))

	marketAddr := pr.cfg.MarketAddress + "_s4"
	_, err = pr.polyEngine.OpenPosition(marketAddr, units, buyPrice, 0, 0)
	return err
}

// S3: two-sided dislocation arbitrage. During fast repricings the two asks
// momentarily sum below $1 minus fees; buying BOTH sides locks a profit that
// pays out regardless of direction (a full YES+NO set is worth exactly $1 at
// settlement, and settlement redemption is free — validated on real account
// data). Trigger is the NET locked profit per pair at executable (sweep) prices
// — not a raw sum threshold — so the minimum profit is guaranteed by
// construction. Measured on 17.7h of clean book snapshots: ~90-100 events/day
// clear the 2-cent floor, avg depth ~3.9c/pair.
//
// PAPER-MODE note on legging risk: live, two FOK orders could half-fill during
// a violent move, leaving a naked directional leg — that emergency path (retry
// missing leg, else bail the filled leg) must be built before this goes live.
// Paper fills are atomic per leg against the real fetched book, so paper
// results measure opportunity frequency/depth, not legging survival.
const (
	s3MinNetPerPair = 0.02  // minimum locked profit per $1-pair, after both entry fees
	s3PairCapital   = 10.00 // total USDC deployed per event (both legs combined, ~$5/side avg)
	s3RetryCooldown = 30 * time.Second
)

var (
	s3LastTryMu sync.Mutex
	s3LastTry   = map[string]time.Time{} // condID -> last pair entry (dedupe)
)

// checkS3Pair is the pure trigger: net locked profit per pair at the given
// executable buy prices, and whether it clears the floor. Unit-testable.
func checkS3Pair(buyYes, buyNo float64) (net float64, ok bool) {
	fee := func(p float64) float64 { return 0.07 * p * (1 - p) }
	net = 1.0 - (buyYes + buyNo) - fee(buyYes) - fee(buyNo)
	return net, net >= s3MinNetPerPair
}

func (pr *PolymarketRunner) tryS3Entry(condID string) error {
	s3LastTryMu.Lock()
	if t, seen := s3LastTry[condID]; seen && time.Since(t) < s3RetryCooldown {
		s3LastTryMu.Unlock()
		return nil
	}
	s3LastTryMu.Unlock()

	estShares := s3PairCapital // ~pair count at sum≈1; close enough for sweep sizing
	_, _, buyYes, okY, errY := pr.polyEngine.EvaluateEntryBook(true, estShares)
	if errY != nil || !okY {
		return nil
	}
	_, _, buyNo, okN, errN := pr.polyEngine.EvaluateEntryBook(false, estShares)
	if errN != nil || !okN {
		return nil
	}

	net, ok := checkS3Pair(buyYes, buyNo)
	if !ok {
		return nil
	}

	// Bug-1 fix (share-count mismatch): round BOTH legs to the $0.01 tick the
	// CLOB actually uses BEFORE sizing. The live path floors each leg to whole
	// shares via floor(units*price / round(price,¢)); with raw sweep prices the
	// two legs' rounding ratios differ and can floor to DIFFERENT counts (e.g.
	// 8 vs 9), leaving a naked, unhedged residual share. With both prices already
	// cent-aligned the ratio is exactly 1, so both legs floor to floor(shares) —
	// identical counts, or one FOK cancels cleanly into the legging handler.
	buyYes = math.Round(buyYes*100) / 100
	buyNo = math.Round(buyNo*100) / 100
	if buyYes <= 0 || buyNo <= 0 || buyYes+buyNo <= 0 {
		return nil
	}

	// Mark the attempt BEFORE opening so a partial failure can't re-fire every
	// tick into the same dislocation.
	s3LastTryMu.Lock()
	s3LastTry[condID] = time.Now()
	if len(s3LastTry) > 50 { // bound: contracts roll every 5 minutes forever
		for k, v := range s3LastTry {
			if time.Since(v) > time.Hour {
				delete(s3LastTry, k)
			}
		}
	}
	s3LastTryMu.Unlock()

	shares := s3PairCapital / (buyYes + buyNo)
	pr.store.Log("INFO", fmt.Sprintf("[S3] Dislocation arb! Pair sum $%.3f (YES $%.3f + NO $%.3f) locks $%.3f/pair after fees — buying %.2f pairs ($%.2f total). Hold to redemption.",
		buyYes+buyNo, buyYes, buyNo, net, shares, s3PairCapital))

	// Leg 1 — YES. If it doesn't fill we have ZERO exposure (FOK either fills
	// fully or cancels); abandon cleanly, no recovery needed.
	yID, errY := pr.polyEngine.OpenPosition(pr.cfg.MarketAddress+"_s3y", shares, buyYes, 0, 0)
	if errY != nil {
		pr.store.Log("INFO", fmt.Sprintf("[S3] YES leg didn't fill (%v) — pair abandoned, no exposure.", errY))
		return nil
	}

	// Leg 2 — NO. Success => fully hedged pair.
	if nID, errN := pr.polyEngine.OpenPosition(pr.cfg.MarketAddress+"_s3n", -shares, buyNo, 0, 0); errN == nil {
		// Belt-and-suspenders: the cent-rounding above should guarantee equal
		// fills, but verify the two legs actually hold matching share counts. A
		// residual here means an unforeseen partial fill left a naked share —
		// surface it loudly so the live watchdog's naked-leg kill-switch trips.
		if yPos, e1 := pr.polyEngine.GetPosition(yID); e1 == nil {
			if nPos, e2 := pr.polyEngine.GetPosition(nID); e2 == nil {
				if d := math.Abs(yPos.Units) - math.Abs(nPos.Units); math.Abs(d) > 1e-6 {
					pr.store.Log("CRITICAL", fmt.Sprintf("[S3] HEDGE IMBALANCE: YES %.4f vs NO %.4f shares (residual %.4f) — naked exposure, review immediately.", math.Abs(yPos.Units), math.Abs(nPos.Units), d))
				}
			}
		}
		return nil
	} else {
		// LEGGING EMERGENCY: YES filled but NO didn't — we now hold a NAKED
		// directional position in the exact volatile moment that created the
		// dislocation. Priority is to stop being directional, fast. This path is
		// live-only: in paper both legs always fill, so it never runs there.
		pr.store.Log("WARN", fmt.Sprintf("[S3] NO leg failed after YES fill (%v) — recovering naked YES leg.", errN))

		// (1) Try once more to complete the hedge at a REFRESHED NO price. Even
		// if the dislocation closed and the pair is now a small loss, being
		// hedged (locked, direction-free) beats holding naked risk.
		if _, _, buyNo2, ok, _ := pr.polyEngine.EvaluateEntryBook(false, shares); ok {
			buyNo2 = math.Round(buyNo2*100) / 100
			if _, err2 := pr.polyEngine.OpenPosition(pr.cfg.MarketAddress+"_s3n", -shares, buyNo2, 0, 0); err2 == nil {
				pr.store.Log("INFO", fmt.Sprintf("[S3] Recovery OK: NO leg filled on retry at $%.3f — pair hedged.", buyNo2))
				return nil
			}
		}

		// (2) Hedge unreachable — FLATTEN the YES leg at the live bid so we exit
		// flat rather than ride a naked bet.
		bail := buyYes
		if bid, okb, errb := pr.polyEngine.GetMarketablePrice(true, 1, shares); errb == nil && okb {
			bail = bid
		}
		pr.store.Log("CRITICAL", fmt.Sprintf("[S3] Recovery failed — flattening naked YES leg %s at $%.3f to kill directional risk.", yID, bail))
		if err := pr.polyEngine.ClosePosition(yID, bail); err != nil {
			pr.store.Log("CRITICAL", fmt.Sprintf("[S3] FLATTEN FAILED (%v) — naked YES leg %s STILL OPEN, manual intervention needed.", err, yID))
		}
	}
	return nil
}
