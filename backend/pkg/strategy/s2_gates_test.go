package strategy

import "testing"

// TestCheckS2Gates locks in the S2 late-window entry thresholds. These are an
// explicit, uncalibrated starting hypothesis (see polymarket_strategy.go) —
// this test pins the current values' behavior so a future recalibration is a
// deliberate, visible change, not an accidental drift.
func TestCheckS2Gates(t *testing.T) {
	cases := []struct {
		name        string
		dist        float64
		prob        float64
		buyPrice    float64
		wantProceed bool
	}{
		{
			name: "decisive, confident, cheap enough — should fire",
			dist: 150, prob: 0.95, buyPrice: 0.90, wantProceed: true,
		},
		{
			name: "too close to strike, even if model is confident",
			dist: 20, prob: 0.95, buyPrice: 0.90, wantProceed: false,
		},
		{
			name: "far from strike but model not confident enough",
			dist: 150, prob: 0.85, buyPrice: 0.90, wantProceed: false,
		},
		{
			name: "decisive and confident but book already priced in — no margin",
			dist: 150, prob: 0.95, buyPrice: 0.97, wantProceed: false,
		},
		{
			name: "right at the distance floor boundary — passes (strict < blocks only below it)",
			dist: 50, prob: 0.95, buyPrice: 0.90, wantProceed: true,
		},
		{
			name: "just under the distance floor boundary — blocked",
			dist: 49.999, prob: 0.95, buyPrice: 0.90, wantProceed: false,
		},
		{
			name: "negative distance (NO side) treated the same as positive",
			dist: -150, prob: 0.95, buyPrice: 0.90, wantProceed: true,
		},
		{
			name: "right at the max entry price boundary — passes (<=)",
			dist: 150, prob: 0.95, buyPrice: s2MaxEntryPrice, wantProceed: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceed, reason := checkS2Gates(c.dist, c.prob, c.buyPrice)
			if proceed != c.wantProceed {
				t.Errorf("proceed=%v want %v (reason=%q)", proceed, c.wantProceed, reason)
			}
			if !proceed && reason == "" {
				t.Error("blocked entry must give a reason")
			}
		})
	}
}
