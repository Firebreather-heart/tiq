package engine

import (
	"math"
	"testing"
)

// TestTakerFee locks in the Crypto-category taker fee formula against
// Polymarket's own documented worked example (docs.polymarket.com/trading/fees):
// 100 shares at $0.50 => $1.75 fee. Also checks the curve peaks at $0.50 and
// shrinks toward the boundaries, matching the documented parabolic shape.
func TestTakerFee(t *testing.T) {
	got := takerFee(100, 0.50)
	want := 1.75
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("takerFee(100, 0.50) = %v, want %v (Polymarket's own worked example)", got, want)
	}

	// Fee should be lower away from $0.50 and zero at the boundaries.
	if takerFee(100, 0.40) >= takerFee(100, 0.50) {
		t.Error("fee at 0.40 should be less than fee at 0.50 (curve peaks at 0.50)")
	}
	if takerFee(100, 0.01) >= takerFee(100, 0.40) {
		t.Error("fee should keep shrinking toward the price boundaries")
	}
	if f := takerFee(100, 0); f != 0 {
		t.Errorf("fee at price=0 should be exactly 0, got %v", f)
	}
	if f := takerFee(100, 1); f != 0 {
		t.Errorf("fee at price=1 should be exactly 0, got %v", f)
	}
}
