package strategy

import "testing"

// TestCheckS4SpotAgreement pins the gate added after S4's first live losses:
// all 3 losses had the book favorite disagreeing with our own BTC spot feed;
// all 8 wins had them agreeing. The gate requires agreement.
func TestCheckS4SpotAgreement(t *testing.T) {
	cases := []struct {
		name          string
		isYes         bool
		spotFavorsYes bool
		wantProceed   bool
	}{
		{"book favors YES, spot agrees — fires", true, true, true},
		{"book favors NO, spot agrees — fires", false, false, true},
		{"book favors YES, spot favors NO — blocked", true, false, false},
		{"book favors NO, spot favors YES — blocked", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceed, reason := checkS4SpotAgreement(c.isYes, c.spotFavorsYes)
			if proceed != c.wantProceed {
				t.Errorf("proceed=%v want %v (reason=%q)", proceed, c.wantProceed, reason)
			}
			if !proceed && reason == "" {
				t.Error("blocked entry must give a reason")
			}
		})
	}
}

// TestCheckS4Price pins the minimum-price gate: all 3 live losses bought at
// $0.54-$0.70 (coin flips); every live win bought at $0.76+. The $0.85 floor
// sits above the entire loss cluster but is deliberately set above the
// cheapest historical win too — a hard floor under every loss, not a threaded
// line between them, so that win is expected to be rejected.
func TestCheckS4Price(t *testing.T) {
	cases := []struct {
		name        string
		buyPrice    float64
		wantProceed bool
	}{
		{"clear favorite, well above floor — fires", 0.97, true},
		{"right at the floor — passes (>=)", s4MinEntryPrice, true},
		{"just under the floor — blocked", s4MinEntryPrice - 0.001, false},
		{"coin flip that lost live ($0.54) — blocked", 0.54, false},
		{"coin flip that lost live ($0.70) — blocked", 0.70, false},
		{"cheapest observed live win ($0.76) — also blocked, floor is deliberately above it", 0.76, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceed, reason := checkS4Price(c.buyPrice)
			if proceed != c.wantProceed {
				t.Errorf("proceed=%v want %v (reason=%q)", proceed, c.wantProceed, reason)
			}
			if !proceed && reason == "" {
				t.Error("blocked entry must give a reason")
			}
		})
	}
}
