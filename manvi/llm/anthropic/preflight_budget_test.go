package anthropic

import (
	"net/http"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// TestThePreflightWaitIsTheFirstTokenBudget binds the adapter, not the
// constants: it records the wait this adapter actually hands the preflight on a
// real request. Hosted anthropic keeps the 15-minute first-token budget (see
// transport.FirstTokenBudget for why), and an edit to preflight.go that passes
// anything else fails here.
func TestThePreflightWaitIsTheFirstTokenBudget(t *testing.T) {
	var waits []time.Duration
	original := preflightStream
	preflightStream = func(resp *http.Response, provider string, wait time.Duration, judge transport.FrameJudge) (transport.StreamAccepted, *transport.Error) {
		waits = append(waits, wait)
		return original(resp, provider, wait, judge)
	}
	t.Cleanup(func() { preflightStream = original })

	adapter, _ := scripted(t, okBody)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 {
		t.Fatalf("the preflight ran %d times, want once", len(waits))
	}
	if want := transport.FirstTokenBudget(transport.DefaultHostedStallTimeout); waits[0] != want || waits[0] != 15*time.Minute {
		t.Fatalf("the adapter passed a preflight wait of %s, want the first-token budget %s (15m)", waits[0], want)
	}
}
