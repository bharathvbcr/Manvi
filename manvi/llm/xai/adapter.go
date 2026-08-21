package xai

import (
	"net/http"

	"manvi/credentials"
	"manvi/llm"
	"manvi/llm/openaicompat"
)

// DefaultMaxTokens bounds a request that did not set its own. Unlike the
// Messages API this field is optional on the wire, but an unbounded request is
// not something a harness should send by default: a runaway generation is
// billed and is not interruptible after the fact.
const DefaultMaxTokens = 8192

// Adapter speaks the OpenAI-compatible chat-completions API.
//
// The wire itself lives in llm/openaicompat, which is shared with the local
// adapter. What stays here is what is actually xAI's: its closed model
// catalogue, its required credential, and its documented base URL. A second
// transcription of the streaming and tool-call logic would be a second place
// for those to drift.
type Adapter struct {
	*openaicompat.Adapter
}

// New builds an adapter.
func New(baseURL string, resolve func() (credentials.Secret, error)) *Adapter {
	return &Adapter{Adapter: openaicompat.New(openaicompat.Options{
		Name:             Name,
		BaseURL:          baseURL,
		DefaultBaseURL:   DefaultBaseURL,
		DefaultMaxTokens: DefaultMaxTokens,
		Validate:         ValidateRequest,
		// reasoning_effort is documented on some xAI models and refused on
		// others; the catalogue decides which, and ValidateRequest has already
		// rejected the impossible combinations by the time a request is built.
		SendReasoningEffort: true,
		Header: func() (http.Header, error) {
			secret, err := resolve()
			if err != nil {
				return nil, err
			}
			h := http.Header{}
			h.Set("Authorization", "Bearer "+secret.Reveal())
			return h, nil
		},
	})}
}

// Capability describes a model this adapter serves.
func (a *Adapter) Capability(model string) (llm.Capability, bool) { return Capability(model) }

// ReplayableOn answers the ReasoningReplayer question for this adapter. The
// OpenAI-compatible wire carries no reasoning field, so nothing is replayable.
func (a *Adapter) ReplayableOn(fromModel, toModel string) bool {
	return ReplayableOn(fromModel, toModel)
}
