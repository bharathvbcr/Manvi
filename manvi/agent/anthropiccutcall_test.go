package agent

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// A response cut off by max_tokens inside a tool call no longer ends the turn:
// the call that arrived whole runs, the cut one is reported to the model, and
// the next request carries both through the real encoder.
func TestACutToolCallIsReportedToTheModelAndTheTurnContinues(t *testing.T) {
	for name, body := range map[string]string{
		"a complete call and a cut call": wireStart +
			wireToolCall(0, "toolu_A", "echo", `"{\"v\":1}"`, true) +
			wireToolCall(1, "toolu_B", "echo", `"{\"v\":"`, true) + wireFinish("max_tokens"),
		"only a cut call": wireStart + wireToolCall(0, "toolu_B", "echo", `"{\"v\":"`, true) + wireFinish("max_tokens"),
	} {
		t.Run(name, func(t *testing.T) {
			reqs, out, err := driveAnthropic(t, []string{body, wireStart + wireAnswer + wireFinish("end_turn")}, tools.Result{Text: "echoed"})
			if err != nil {
				t.Fatalf("turn died: %v", err)
			}
			if out.Malformed != 1 {
				t.Errorf("Malformed = %d, want 1", out.Malformed)
			}
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want 2 (the turn continued)", len(reqs))
			}
			if !strings.Contains(reqs[1], "could not read") || !strings.Contains(reqs[1], "output limit") {
				t.Errorf("the model was not told its call was cut off:\n%s", reqs[1])
			}
			if strings.Contains(reqs[1], `"id":"toolu_B"`) {
				t.Errorf("the cut call was replayed as a tool_use:\n%s", reqs[1])
			}
		})
	}
}
