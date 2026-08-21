package agent

import (
	"testing"

	"manvi/llm"
)

// Measured against Qwen3.8-27B's own tokenizer, the byte heuristic runs about
// 25% high overall and 58% high on JSON. Uncorrected, compaction fires that
// much earlier than it needs to and destroys context the model could have kept.
func TestCalibrationConvergesOnTheServersCount(t *testing.T) {
	var c Calibrator
	if c.Ratio() != 1 {
		t.Fatalf("an uncalibrated ratio must be 1, got %v", c.Ratio())
	}

	// The harness estimates 25870 where the tokenizer counts 20680.
	for i := 0; i < 8; i++ {
		c.Observe(25870, 20680)
	}
	want := 20680.0 / 25870.0
	if got := c.Ratio(); got < want-0.01 || got > want+0.01 {
		t.Fatalf("ratio = %.4f, want ~%.4f", got, want)
	}
	if got := c.Calibrated(10000); got > 8100 || got < 7900 {
		t.Fatalf("Calibrated(10000) = %d, want ~8000", got)
	}
	if c.Samples() != 8 {
		t.Fatalf("Samples = %d", c.Samples())
	}
}

func TestOneOddSampleDoesNotSwingTheBudget(t *testing.T) {
	var c Calibrator
	for i := 0; i < 10; i++ {
		c.Observe(1000, 800)
	}
	steady := c.Ratio()

	// A step whose prompt was reshaped by something the estimator cannot see.
	c.Observe(1000, 1600)
	moved := c.Ratio() - steady
	if moved > 0.25 {
		t.Fatalf("a single sample moved the ratio by %.3f; compaction would fire at a "+
			"different point each step for no visible reason", moved)
	}
}

func TestNonsenseObservationsAreRejected(t *testing.T) {
	var c Calibrator
	for _, tc := range []struct{ est, actual int }{
		{0, 100},      // nothing was estimated
		{100, 0},      // the server reported nothing
		{-5, 100},     // impossible
		{100, 100000}, // a cumulative counter, not this request
		{100000, 100}, // a rejected request
	} {
		c.Observe(tc.est, tc.actual)
	}
	if c.Samples() != 0 {
		t.Fatalf("accepted %d nonsense observation(s); a budget derived from one is "+
			"worse than an uncalibrated budget", c.Samples())
	}
	if c.Ratio() != 1 {
		t.Fatalf("ratio = %v after only nonsense", c.Ratio())
	}
}

func TestANilCalibratorIsUsable(t *testing.T) {
	var c *Calibrator
	if c.Ratio() != 1 || c.Samples() != 0 {
		t.Fatal("a nil calibrator must behave as uncalibrated rather than panic")
	}
}

// A calibrated estimate that the server says is smaller than the heuristic
// thought means more history fits, so compaction should do strictly less work.
func TestCalibrationLetsMoreHistorySurvive(t *testing.T) {
	msgs := history(24, 40)
	budget := testBudget(8192)

	uncalibrated := PlanCompactionCalibrated(msgs, "sys", nil, budget, map[llm.CallID]struct{}{}, nil)

	var c Calibrator
	for i := 0; i < 8; i++ {
		c.Observe(1000, 700) // the tokenizer counts 30% fewer than estimated
	}
	calibrated := PlanCompactionCalibrated(msgs, "sys", nil, budget, map[llm.CallID]struct{}{}, &c)

	if len(calibrated.Steps) > len(uncalibrated.Steps) {
		t.Fatalf("calibration made compaction more aggressive (%d vs %d steps) despite the "+
			"server counting fewer tokens", len(calibrated.Steps), len(uncalibrated.Steps))
	}
	if calibrated.Before >= uncalibrated.Before {
		t.Fatalf("calibrated estimate %d not below uncalibrated %d",
			calibrated.Before, uncalibrated.Before)
	}
}
