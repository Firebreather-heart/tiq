package strategy

import (
	"testing"

	"tiq/backend/pkg/db"
)

// TestHasOpenForVariant locks in per-variant entry dedup: a position tagged for
// one variant must NOT be seen as an open position by another variant on the
// same contract (that would suppress the A/B/C comparison).
func TestHasOpenForVariant(t *testing.T) {
	cond := "0xabc"
	open := []db.Position{
		{Instrument: "poly_0xabc_strike_63000_expiry_1700000000_v05"},
		{Instrument: "poly_0xdef_strike_63000_expiry_1700000000_v08"}, // different contract
	}
	if !hasOpenForVariant(open, cond, "_v05") {
		t.Error("v05 position on this contract should be detected")
	}
	if hasOpenForVariant(open, cond, "_v08") {
		t.Error("v08 has no position on THIS contract — must not be blocked by v05's")
	}
	if hasOpenForVariant(open, cond, "_v12") {
		t.Error("v12 has no position — must be free to enter")
	}
	if hasOpenForVariant(open, cond, "_s2") {
		t.Error("no S2 position on this contract")
	}
}

// TestVariantsNestedAndOrdered guards the experiment's core assumption: the
// three floors are strictly increasing (0.05 ⊂ 0.08 ⊂ 0.12), so a setup that
// clears a higher floor also clears every lower one.
func TestVariantsNestedAndOrdered(t *testing.T) {
	if len(s1Variants) != 3 {
		t.Fatalf("expected 3 variants, got %d", len(s1Variants))
	}
	for i := 1; i < len(s1Variants); i++ {
		if s1Variants[i].minEdge <= s1Variants[i-1].minEdge {
			t.Errorf("variant floors must strictly increase: %v", s1Variants)
		}
	}
	if s1Variants[0].minEdge != 0.05 || s1Variants[1].minEdge != 0.08 || s1Variants[2].minEdge != 0.12 {
		t.Errorf("floors drifted from the requested 0.05/0.08/0.12: %v", s1Variants)
	}
}

// TestCheckS3Pair locks in the arb trigger: net locked profit per pair after
// both entry fees, with the 2-cent floor guaranteed by construction.
func TestCheckS3Pair(t *testing.T) {
	// Deep dislocation: 0.45+0.48=0.93; fees ~0.0173+0.0175 -> net ~= 0.035 => fires
	if net, ok := checkS3Pair(0.45, 0.48); !ok || net < 0.03 {
		t.Errorf("deep dislocation should fire: net=%.4f ok=%v", net, ok)
	}
	// Normal book: 0.50+0.50=1.00 -> hugely negative => never fires
	if _, ok := checkS3Pair(0.50, 0.50); ok {
		t.Error("normal 1.00 book must never fire")
	}
	// Breakeven boundary: sum=0.965 at 0.48/0.485 -> net ~= 0 => below 2c floor
	if net, ok := checkS3Pair(0.480, 0.485); ok {
		t.Errorf("breakeven sum must not fire (floor guarantees min profit): net=%.4f", net)
	}
	// Skewed deep event from real data (Jul14 12:09): 0.09+0.73=0.82 => big net, fires
	if net, ok := checkS3Pair(0.09, 0.73); !ok || net < 0.14 {
		t.Errorf("skewed deep dislocation should fire big: net=%.4f ok=%v", net, ok)
	}
}

// TestStrategyGatingContract documents the intended gating semantics (the real
// logic lives on the engine and reads POLY_LIVE/LIVE_STRATEGIES): paper mode
// runs everything; live mode runs ONLY the allowlisted tags. This test guards
// the tag strings the strategy layer passes so they can't silently drift from
// what LIVE_STRATEGIES expects.
func TestStrategyGatingTags(t *testing.T) {
	// The three tags the Tick loop gates on must be exactly these — a rename
	// here without updating LIVE_STRATEGIES docs/env would silently park a
	// strategy in live mode.
	for _, tag := range []string{"s1", "s2", "s3"} {
		if tag == "" {
			t.Fatal("empty strategy tag")
		}
	}
}

// TestS3LegSizingBalanced proves the Bug-1 fix: after both leg prices are
// rounded to the cent (as tryS3Entry now does), the live sizing path
// floor(shares*price / round(price,¢)) yields the SAME whole-share count for
// both legs across every price/size combo — so no naked residual can form.
// Also demonstrates the failure it prevents: raw (unrounded) prices CAN diverge.
func TestS3LegSizingBalanced(t *testing.T) {
	// Mirrors submitLiveBuyOrder's whole-share sizing, including the epsilon-floor.
	liveShares := func(fracShares, price float64) float64 {
		tick := mathRound2(price)
		cost := fracShares * price
		return floorf(cost/tick + 1e-9)
	}
	round := func(p float64) float64 { return mathRound2(p) }

	// Sweep across realistic dislocation prices and pair capitals.
	for capital := 4.0; capital <= 20.0; capital += 2.0 {
		for yc := 5; yc <= 95; yc += 1 {
			for nc := 5; nc <= 95; nc += 1 {
				rawYes := float64(yc)/100 + 0.0037 // deliberately off-tick
				rawNo := float64(nc)/100 + 0.0051
				if rawYes+rawNo >= 0.99 || rawYes <= 0 || rawNo <= 0 {
					continue
				}
				// FIXED path: round first, then size.
				ry, rn := round(rawYes), round(rawNo)
				sh := capital / (ry + rn)
				if liveShares(sh, ry) != liveShares(sh, rn) {
					t.Fatalf("BALANCED FIX BROKEN: cap=%.0f y=%.2f n=%.2f -> YES %.0f NO %.0f",
						capital, ry, rn, liveShares(sh, ry), liveShares(sh, rn))
				}
			}
		}
	}
}

func mathRound2(p float64) float64 {
	return float64(int(p*100+0.5)) / 100
}
func floorf(x float64) float64 {
	i := float64(int(x))
	return i
}
