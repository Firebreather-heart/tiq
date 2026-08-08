package strategy

import "testing"

// TestCheckS2Gates pins the RE-CALIBRATED (buffer-strategy) S2 gates: buffer
// >= $30 (skip the <30 coin-flip trap), and the favorite priced in the
// [s2MinEntryPrice, s2MaxEntryPrice] band (a real decisive favorite with margin
// left to the $1.00 redemption). No Black-Scholes-probability gate anymore —
// the historical reconstruction validated buffer + price, not the model.
func TestCheckS2Gates(t *testing.T) {
	cases := []struct {
		name        string
		dist        float64
		buyPrice    float64
		wantProceed bool
	}{
		{"decisive buffer, ~0.90 favorite — fires", 45, 0.90, true},
		{"inside the <30 trap zone — blocked", 20, 0.90, false},
		{"buffer at $30 floor — passes (strict < blocks only below)", 30, 0.90, true},
		{"just under the $30 floor — blocked", 29.999, 0.90, false},
		{"decisive buffer but book fully priced in (no margin) — blocked", 60, 0.97, false},
		{"decisive buffer but favorite too cheap (market disagrees) — blocked", 40, 0.70, false},
		{"negative buffer (NO side) treated same as positive", -45, 0.90, true},
		{"right at the max price boundary — passes (<=)", 50, s2MaxEntryPrice, true},
		{"right at the min price boundary — passes (>=)", 50, s2MinEntryPrice, true},
		{"just under min price — blocked", 50, s2MinEntryPrice - 0.001, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceed, reason := checkS2Gates(c.dist, c.buyPrice)
			if proceed != c.wantProceed {
				t.Errorf("proceed=%v want %v (reason=%q)", proceed, c.wantProceed, reason)
			}
			if !proceed && reason == "" {
				t.Error("blocked entry must give a reason")
			}
		})
	}
}
