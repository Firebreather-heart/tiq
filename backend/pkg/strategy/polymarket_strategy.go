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

	// Limit trade entry to the first 2 minutes of the active contract (between 300s and 180s remaining)
	if timeRemaining < 180.0 || timeRemaining > 300.0 {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Time remaining (%.0fs) is outside the first 2 minutes entry window (180s to 300s). Skipping trade.", timeRemaining))
		return nil
	}

	// Dynamically scale minimum lag using ATR: Require lag >= 0.75 * ATR (with a $20 floor)
	minLag := 0.75 * atr
	if minLag < 20.0 {
		minLag = 20.0
	}

	lag := math.Abs(currentPrice - pr.cfg.StrikePrice)
	if lag < minLag {
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Current lag ($%.2f) is less than the required dynamic minimum of $%.2f (0.75 * ATR). Skipping trade.", lag, minLag))
		return nil
	}

	pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Evaluating Market %s. Expiry in %.0fs. Current Spot: $%.2f | Dynamic Min Lag: $%.2f", 
		pr.cfg.MarketAddress, timeRemaining, currentPrice, minLag))

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

	// 4. Position Check
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

			profitPercent := (currentSharePrice - pos.OpenPrice) / pos.OpenPrice
			
			// Update the price of the actual open position's instrument ID (YES price) in the engine feed
			_ = pr.polyEngine.UpdatePrices(map[string]float64{
				pos.Instrument: marketYesPrice,
			})
			if profitPercent >= 0.20 {
				pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Take Profit triggered! Share Price: %.2f (Entry: %.2f, Profit: %.1f%% >= 20%%). Closing position.", 
					currentSharePrice, pos.OpenPrice, profitPercent*100))
				err = pr.polyEngine.ClosePosition(pos.ID, currentSharePrice)
				return err
			}
			return nil 
		}
	}

	// 5. Open Position if Expected Value exceeds our Edge target
	_, _, err = pr.polyEngine.GetBalance()
	if err != nil {
		return err
	}

	// Fixed risk of $5 USDC per trade as requested by the user
	riskCapital := 5.00

	if yesEV >= pr.cfg.MinExpectedValue {
		// Boundary check: Only enter YES if price is between $0.40 and $0.60
		if marketYesPrice < 0.40 || marketYesPrice > 0.60 {
			pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected, but YES price ($%.2f) is outside the $0.40 - $0.60 entry boundaries. Skipping YES trade.", marketYesPrice))
			return nil
		}

		// Trend momentum check: Only enter YES if short-term EMA trend is bullish
		if !isBullishTrend {
			pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected, but short-term EMA trend is bearish. Skipping YES trade. Spot: $%.2f, Strike: $%.2f", currentPrice, pr.cfg.StrikePrice))
			return nil
		}

		// Buy YES tokens
		units := riskCapital / marketYesPrice
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected! Buying %.2f YES tokens. Risk USDC: $%.2f", units, riskCapital))
		
		// Set dynamic limits: SL = entryPrice - 0.03 (3-cent buffer to avoid premature stop-outs), TP = marketYesPrice * 1.20 (20% Profit level)
		_, err = pr.polyEngine.OpenPosition(targetInstrument, units, marketYesPrice, marketYesPrice - 0.03, marketYesPrice * 1.20)
		if err != nil {
			return err
		}
	} else if noEV >= pr.cfg.MinExpectedValue {
		// Boundary check: Only enter NO if price is between $0.40 and $0.60
		if marketNoPrice < 0.40 || marketNoPrice > 0.60 {
			pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected, but NO price ($%.2f) is outside the $0.40 - $0.60 entry boundaries. Skipping NO trade.", marketNoPrice))
			return nil
		}

		// Trend momentum check: Only enter NO if short-term EMA trend is bearish (not bullish)
		if isBullishTrend {
			pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected, but short-term EMA trend is bullish. Skipping NO trade. Spot: $%.2f, Strike: $%.2f", currentPrice, pr.cfg.StrikePrice))
			return nil
		}

		// Buy NO tokens (represented as negative units)
		units := -riskCapital / marketNoPrice
		pr.store.Log("INFO", fmt.Sprintf("[Polymarket Strategy] Edge detected! Buying %.2f NO tokens. Risk USDC: $%.2f", math.Abs(units), riskCapital))
		
		// Set dynamic limits: SL = entryPrice - 0.03 (3-cent buffer to avoid premature stop-outs), TP = marketNoPrice * 1.20 (20% Profit level)
		_, err = pr.polyEngine.OpenPosition(targetInstrument, units, marketNoPrice, marketNoPrice - 0.03, marketNoPrice * 1.20)
		if err != nil {
			return err
		}
	} else {
		pr.store.Log("INFO", "[Polymarket Strategy] Expected Value edge insufficient. HOLD/Wait.")
	}

	return nil
}

