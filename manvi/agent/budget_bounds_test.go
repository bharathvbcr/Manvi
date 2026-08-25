package agent

import (
	"math"
	"testing"
)

// A budget is three numbers a caller supplies, and two of the three can be
// hostile: the serve plane takes them off the wire, and a provider's declared
// context window is a value from a response body. The arithmetic over them has
// to hold for every input, not for the ones anybody thought to try.
//
// It did not. Target multiplied the threshold by seven before dividing, so a
// threshold near the top of the range wrapped — and a *negative* target is a
// compaction goal the planner reads as already met, which is the failure mode
// where the harness stops compacting exactly when it most needs to.
func TestBudgetArithmeticHoldsForEveryInput(t *testing.T) {
	extremes := []int{
		math.MinInt, math.MinInt + 1, math.MinInt / 2,
		-1 << 40, -1000000, -4096, -1, 0, 1, 4095, 4096, 4097,
		1 << 20, 1 << 30, 1 << 40, 1 << 60,
		math.MaxInt / 2, math.MaxInt - 4096, math.MaxInt,
	}
	for _, window := range extremes {
		for _, reserved := range extremes {
			for _, overhead := range []int{0, -1, 1 << 20, math.MaxInt, math.MinInt} {
				b := Budget{ContextWindow: window, ReservedOutput: reserved, Overhead: overhead}

				threshold := b.Threshold()
				if threshold < minThreshold {
					t.Fatalf("Threshold() = %d for %+v; the floor must hold", threshold, b)
				}
				if threshold > maxThreshold {
					t.Fatalf("Threshold() = %d for %+v; the ceiling must hold", threshold, b)
				}

				target := b.Target()
				if target <= 0 {
					t.Fatalf("Target() = %d for %+v; a non-positive target reads as already met, "+
						"so compaction would stop exactly when it is needed", target, b)
				}
				if target > threshold {
					t.Fatalf("Target() = %d exceeds Threshold() = %d for %+v", target, threshold, b)
				}
				// The ratio is the point of the value; a wrap that happened to
				// stay positive would still be wrong.
				want := threshold * CompactionHeadroomNum / CompactionHeadroomDen
				if target != want {
					t.Fatalf("Target() = %d for %+v, want %d — the headroom ratio did not survive",
						target, b, want)
				}
			}
		}
	}
}

// The ordinary case is unchanged: a real budget still produces the same numbers
// it always did, so the clamps above are a backstop rather than a behaviour
// change anybody will notice.
func TestOrdinaryBudgetsAreUnaffected(t *testing.T) {
	for _, tc := range []struct {
		b                 Budget
		threshold, target int
	}{
		{Budget{ContextWindow: 200000, ReservedOutput: 8192, Overhead: 2000}, 189808, 132865},
		{Budget{ContextWindow: 8192, ReservedOutput: 1024, Overhead: 256}, 6912, 4838},
		{Budget{ContextWindow: 4096, ReservedOutput: 0, Overhead: 0}, 4096, 2867},
	} {
		if got := tc.b.Threshold(); got != tc.threshold {
			t.Errorf("Threshold() = %d for %+v, want %d", got, tc.b, tc.threshold)
		}
		if got := tc.b.Target(); got != tc.target {
			t.Errorf("Target() = %d for %+v, want %d", got, tc.b, tc.target)
		}
	}
}
