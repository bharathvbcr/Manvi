package budget

import (
	"math/rand/v2"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

var tiered = Prices{
	Rates:             Rates{Input: 100, Output: 500, CacheWrite: 125, CacheRead: 10},
	LongPromptAbove:   100_000,
	LongRates:         Rates{Input: 500, Output: 2_500, CacheWrite: 625, CacheRead: 50},
	ReasoningInOutput: true,
}

func TestChargePricesEachPartOfThePrompt(t *testing.T) {
	got, err := tiered.Charge(llm.Usage{InputTokens: 1_000, CacheReadTokens: 600, CacheWriteTokens: 300, OutputTokens: 20, ReasoningTokens: 15})
	if err != nil {
		t.Fatal(err)
	}
	// 100 uncached*100 + 300*125 + 600*10 + 20*500; reasoning is inside output.
	if want := int64(10_000 + 37_500 + 6_000 + 10_000); got != want {
		t.Fatalf("charge = %d, want %d", got, want)
	}
	separate := tiered
	separate.ReasoningInOutput = false
	got, err = separate.Charge(llm.Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 15})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(10*100 + 35*500); got != want {
		t.Fatalf("separately reported reasoning: charge = %d, want %d", got, want)
	}
}

func TestChargeSwitchesTierOnTheWholePrompt(t *testing.T) {
	short, err := tiered.Charge(llm.Usage{InputTokens: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	long, err := tiered.Charge(llm.Usage{InputTokens: 100_001, CacheReadTokens: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	if short != 100_000*100 || long != 1*500+100_000*50 {
		t.Fatalf("short=%d long=%d", short, long)
	}
}

func TestChargeRefusesUsageItCannotPrice(t *testing.T) {
	for _, u := range []llm.Usage{
		{},
		{InputTokens: 10, CacheReadTokens: 11},
		{InputTokens: 10, CacheReadTokens: 6, CacheWriteTokens: 6},
		{InputTokens: 10, OutputTokens: -1},
		{InputTokens: 10, CacheReadTokens: -1},
		{InputTokens: maxTokens + 1},
	} {
		if _, err := tiered.Charge(u); err == nil {
			t.Errorf("priced invalid usage %+v", u)
		}
	}
}

func TestValidateRefusesTablesThatCannotBoundACharge(t *testing.T) {
	for _, p := range []Prices{
		{},
		{Rates: Rates{Input: 1, Output: 1, CacheWrite: 1}},
		{Rates: Rates{Input: 1, Output: 1, CacheWrite: 1, CacheRead: maxRate + 1}},
		{Rates: Rates{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 1}, LongPromptAbove: 10},
		{Rates: Rates{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 1}, LongPromptAbove: -1},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

// TestReserveBoundsEveryChargeWithinItsLimits is the property the ledger rests
// on: no usage that fits the bounds a reservation was sized for can cost more
// than the reservation, whichever way the prompt splits between fresh input,
// cache writes and cache reads, and on either side of a tier threshold.
func TestReserveBoundsEveryChargeWithinItsLimits(t *testing.T) {
	separate := tiered
	separate.ReasoningInOutput = false
	flat := Prices{Rates: Rates{Input: 4_000, Output: 20_000, CacheWrite: 5_000, CacheRead: 200}, ReasoningInOutput: true}
	// #nosec G404 -- a fixed-seed sweep: reproducing a failing draw is the point, secrecy is not.
	rng := rand.New(rand.NewPCG(1, 2))
	for _, p := range []Prices{tiered, separate, flat} {
		for range 20_000 {
			promptBound := 1 + rng.IntN(300_000)
			outputBound := 1 + rng.IntN(128_000)
			reserve, err := p.Reserve(promptBound, outputBound)
			if err != nil {
				t.Fatal(err)
			}
			input := 1 + rng.IntN(promptBound)
			read := rng.IntN(input + 1)
			write := rng.IntN(input - read + 1)
			output := rng.IntN(outputBound + 1)
			u := llm.Usage{InputTokens: input, CacheReadTokens: read, CacheWriteTokens: write, OutputTokens: output}
			if p.ReasoningInOutput {
				u.ReasoningTokens = rng.IntN(output + 1)
			} else {
				u.ReasoningTokens = rng.IntN(outputBound - output + 1)
			}
			charge, err := p.Charge(u)
			if err != nil {
				t.Fatal(err)
			}
			if charge > reserve {
				t.Fatalf("usage %+v costs %d, above the %d reserved for %d/%d tokens", u, charge, reserve, promptBound, outputBound)
			}
		}
	}
}

func TestReserveRefusesBoundsOutsideRange(t *testing.T) {
	for _, b := range [][2]int{{0, 1}, {1, 0}, {maxTokens + 1, 1}, {1, -1}} {
		if _, err := tiered.Reserve(b[0], b[1]); err == nil {
			t.Errorf("reserved for bounds %v", b)
		}
	}
}
