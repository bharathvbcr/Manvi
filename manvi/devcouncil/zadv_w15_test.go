package devcouncil

import (
	"strings"
	"testing"
)

// Adversarial review of lane w15-workwire: the run id and session id in every
// audit line and request id are the host's, never anything the model wrote.

// A model that puts run_id, session_id, request_id or owner in a board call must
// be refused as an unknown field, and nothing may be written for it; the next
// honest write must still carry the host's run id.
func TestAdvW15ModelCannotSupplyRunOrSessionIdentity(t *testing.T) {
	env := newWorkEnv(t, "hostrun")
	env.checkout("TASK-001")
	own := env.item("TASK-001")
	puts := env.putCalls()
	for _, field := range []string{"run_id", "session_id", "request_id", "owner", "actor"} {
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"work_create_item", map[string]any{"title": "x", field: "forged"}},
			{"work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x", field: "forged"}},
		} {
			res := env.call(tc.tool, tc.args)
			if !res.IsError {
				t.Errorf("%s accepted a model-supplied %s: %s", tc.tool, field, res.Text)
			}
		}
	}
	if got := env.putCalls(); got != puts {
		t.Errorf("a forged identity field caused %d write(s)", got-puts)
	}
	out := env.success(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "honest"}))
	req, _ := out["request_id"].(string)
	if !strings.HasPrefix(req, "agent_hostrun_s1_") {
		t.Errorf("request id %q does not carry the host's run id", req)
	}
	if strings.Contains(env.item("TASK-001").Logs, "forged") {
		t.Error("a forged identity reached the audit log")
	}
}
