package strategy

import "testing"

// TestCheckEntryGates locks in the guardrails added after the 16:11 live loss.
// The loss trade must be blocked; legitimate small-lag trades must still pass.
func TestCheckEntryGates(t *testing.T) {
	const minEdge = 0.12

	cases := []struct {
		name        string
		trueProb    float64
		bestBid     float64
		bestAsk     float64
		buyPrice    float64
		wantProceed bool
	}{
		{
			// The actual loss trade: model 90%, book bid ~0.25 / ask 0.65 (spread 0.40),
			// bought at 0.65. Wide-book gate fires first — must be blocked.
			name: "16:11 loss trade — wide book", trueProb: 0.90,
			bestBid: 0.25, bestAsk: 0.65, buyPrice: 0.65, wantProceed: false,
		},
		{
			// Even on a tight book, a 0.26 edge is an implausible model/market gap.
			name: "outsized edge on tight book", trueProb: 0.90,
			bestBid: 0.63, bestAsk: 0.65, buyPrice: 0.64, wantProceed: false,
		},
		{
			// Edge evaporated at the real price — repriced, skip.
			name: "edge below floor", trueProb: 0.60,
			bestBid: 0.55, bestAsk: 0.57, buyPrice: 0.57, wantProceed: false,
		},
		{
			// Real executable price outside the boundary band.
			name: "price above boundary", trueProb: 0.82,
			bestBid: 0.66, bestAsk: 0.68, buyPrice: 0.67, wantProceed: false,
		},
		{
			// A genuine small latency lag on a tight two-sided book: 0.15 edge,
			// buy at 0.50, spread 0.02. This is exactly what we DO want to trade.
			name: "legit small-lag scalp", trueProb: 0.65,
			bestBid: 0.49, bestAsk: 0.51, buyPrice: 0.50, wantProceed: true,
		},
		{
			// Edge right at the ceiling is allowed (0.20 with buy 0.45, prob 0.65).
			name: "edge at ceiling ok", trueProb: 0.65,
			bestBid: 0.44, bestAsk: 0.46, buyPrice: 0.45, wantProceed: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceed, edge, reason := checkEntryGates(c.trueProb, c.bestBid, c.bestAsk, c.buyPrice, minEdge)
			if proceed != c.wantProceed {
				t.Errorf("proceed=%v want %v (edge=%.3f reason=%q)", proceed, c.wantProceed, edge, reason)
			}
			if !proceed && reason == "" {
				t.Error("blocked trade must give a reason")
			}
		})
	}
}
