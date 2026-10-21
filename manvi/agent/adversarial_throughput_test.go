package agent

import (
	"context"
	"math"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
)

// nonFiniteProvider wraps the replay provider and, as an adapter that derives
// tokens-per-second by dividing by a zero elapsed time would, stamps a
// non-finite throughput onto each settled response.
type nonFiniteProvider struct {
	*replay.Provider
	rate float64
}

type nonFiniteStream struct {
	llm.Stream
	rate float64
}

func (p nonFiniteProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	s, err := p.Provider.Stream(ctx, req)
	return nonFiniteStream{Stream: s, rate: p.rate}, err
}

func (s nonFiniteStream) Response() (llm.Response, error) {
	r, err := s.Stream.Response()
	r.Usage.OutputTokensPerSecond = s.rate
	return r, err
}

// The step/end payload carries llm.Usage whole, throughput floats included. A
// NaN or +Inf there is a value json cannot encode, so Append fails and endStep
// returns the error: a step that was paid for and answered ends the turn with
// an error. Before this change step/end had no payload and the same response
// was harmless.
func TestAdversarialNonFiniteThroughputDoesNotFailAnAnsweredStep(t *testing.T) {
	for name, v := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1)} {
		h := build(t, []replay.Turn{textTurn("done")}, nil)
		h.loop.cfg.Provider = nonFiniteProvider{Provider: h.provider, rate: v}
		out, err := h.loop.Run(context.Background(), userMsg("go"))
		if err != nil {
			t.Errorf("%s throughput: the turn failed with %v after the model answered (final=%q)", name, err, out.Final.Text())
		}
	}
}
