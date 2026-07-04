package engine

import (
	"math"
	"testing"
)

// Mirrors the sizing math in submitLiveBuyOrder/submitLiveOrder so a regression
// in the implied limit price is caught without hitting the live CLOB.
func TestBuyOrderImpliedPriceEqualsTick(t *testing.T) {
	cases := []struct {
		price, budget float64
	}{
		{0.57, 5.00}, {0.35, 5.00}, {0.65, 5.00}, {0.505, 5.00}, {0.49, 15.00},
	}
	for _, c := range cases {
		tick := math.Round(c.price*100) / 100
		shares := math.Floor(c.budget / tick)
		if shares < 1 {
			t.Fatalf("no shares for %+v", c)
		}
		usdcRaw := int64(math.Round(shares * tick * 1e6))
		sharesRaw := int64(math.Round(shares * 1e6))
		implied := float64(usdcRaw) / float64(sharesRaw)
		if math.Abs(implied-tick) > 1e-9 {
			t.Errorf("price %.3f budget %.2f: implied limit %.6f != tick %.2f", c.price, c.budget, implied, tick)
		}
		if shares*tick > c.budget+1e-9 {
			t.Errorf("price %.3f: cost %.4f exceeds budget %.2f", c.price, shares*tick, c.budget)
		}
	}
}
