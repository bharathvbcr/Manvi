package budget

import (
	"errors"
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Rates are one price tier, in nano-USD per token. $1 per million tokens is
// 1000 nano-USD per token, so every published rate is a whole number here.
type Rates struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
}

// Prices is what one model costs. The provider owns the figures; this package
// owns the arithmetic, so a charge is computed one way whichever provider
// supplied the table.
type Prices struct {
	Rates
	// LongPromptAbove, when positive, switches every rate to LongRates for a
	// request whose prompt exceeds it. A model priced flat leaves it zero.
	LongPromptAbove int   `json:"long_prompt_above,omitempty"`
	LongRates       Rates `json:"long_rates,omitempty"`
	// ReasoningInOutput says the provider's output count already includes
	// reasoning tokens. Providers disagree on this, and adding reasoning on top
	// of an output count that contains it bills thinking twice.
	ReasoningInOutput bool `json:"reasoning_in_output,omitempty"`
}

// Pricer is implemented by a provider that publishes per-model prices. A
// budgeted provider without prices is refused: a cap that cannot price a
// request cannot enforce anything.
type Pricer interface {
	PriceOf(model string) (Prices, bool)
}

const (
	maxRate   = 1_000_000  // $1000 per million tokens
	maxTokens = 10_000_000 // above any context window or output cap served
)

func (r Rates) validate() error {
	for _, v := range []int64{r.Input, r.Output, r.CacheWrite, r.CacheRead} {
		if v <= 0 || v > maxRate {
			return fmt.Errorf("budget: rate %d nano-USD/token outside (0, %d]", v, maxRate)
		}
	}
	return nil
}

// Validate refuses a table that could not bound a charge.
func (p Prices) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.LongPromptAbove < 0 {
		return errors.New("budget: negative long-prompt threshold")
	}
	if p.LongPromptAbove > 0 {
		return p.LongRates.validate()
	}
	return nil
}

func (p Prices) tier(promptTokens int) Rates {
	if p.LongPromptAbove > 0 && promptTokens > p.LongPromptAbove {
		return p.LongRates
	}
	return p.Rates
}

// UnbilledRejector is implemented by a provider that can say an HTTP status
// proves its request was refused before any billable work — rate limited,
// overloaded, malformed, unauthorised. A rejection it does not vouch for keeps
// its reservation.
type UnbilledRejector interface {
	RejectionUnbilled(status int) bool
}

// UsageReporter is implemented by a stream that can report the usage it has
// seen so far. For a stream cut short, the prompt side of that usage is
// final, which bounds the charge far more tightly than the reservation did.
type UsageReporter interface {
	UsageSoFar() (llm.Usage, bool)
}

// Bound is an upper bound on what an attempt can have cost when its prompt
// usage is known and its output is not: the known prompt, plus every output
// token the request allowed.
func (p Prices) Bound(prompt llm.Usage, outputBound int) (int64, error) {
	if outputBound <= 0 || outputBound > maxTokens {
		return 0, fmt.Errorf("budget: output bound %d outside (0, %d]", outputBound, maxTokens)
	}
	prompt.OutputTokens, prompt.ReasoningTokens = outputBound, 0
	return p.Charge(prompt)
}

// Reserve bounds what one attempt can cost before it is sent: every prompt
// token priced at the dearer of a fresh read and a cache write, and every
// output token the request allows.
func (p Prices) Reserve(promptBound, outputBound int) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if promptBound <= 0 || promptBound > maxTokens || outputBound <= 0 || outputBound > maxTokens {
		return 0, fmt.Errorf("budget: token bounds %d/%d outside (0, %d]", promptBound, outputBound, maxTokens)
	}
	r := p.tier(promptBound)
	in := max(r.Input, r.CacheWrite)
	return int64(promptBound)*in + int64(outputBound)*r.Output, nil
}

// Charge prices authoritative usage. llm.Usage.InputTokens is the whole
// prompt, cached parts included, so the uncached remainder is what is left
// after the cache reads and writes are taken out.
func (p Prices) Charge(u llm.Usage) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if u.InputTokens <= 0 || u.InputTokens > maxTokens || u.OutputTokens < 0 || u.OutputTokens > maxTokens ||
		u.ReasoningTokens < 0 || u.ReasoningTokens > maxTokens || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		return 0, fmt.Errorf("budget: provider usage missing or out of range: %+v", u)
	}
	uncached := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if uncached < 0 {
		return 0, fmt.Errorf("budget: cached tokens (%d read, %d written) exceed the %d-token prompt",
			u.CacheReadTokens, u.CacheWriteTokens, u.InputTokens)
	}
	r := p.tier(u.InputTokens)
	output := int64(u.OutputTokens)
	if !p.ReasoningInOutput {
		output += int64(u.ReasoningTokens)
	}
	return int64(uncached)*r.Input + int64(u.CacheWriteTokens)*r.CacheWrite +
		int64(u.CacheReadTokens)*r.CacheRead + output*r.Output, nil
}
