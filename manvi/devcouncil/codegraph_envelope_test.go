package devcouncil

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

const sharedFreshness = "whole-tree source freshness was not checked for this answer"

// A real explore repeated source_freshness once per sub-envelope. Through the
// tool it now appears once, at the top, and the walk_incomplete values that
// differ are all still there.
func TestCodeAnswerHoistsIdenticalEnvelopeFields(t *testing.T) {
	f := newFixture(t)
	s := newStubIndex(t, f, 4, true)
	s.set(t, "explore", realExplore(t))

	res := f.call(codeToolName, map[string]string{"kind": "explore", "query": "codeQuery"})
	if res.IsError {
		t.Fatalf("explore failed: %s", res.Text)
	}
	if got := strings.Count(res.Text, sharedFreshness); got != 1 {
		t.Errorf("source_freshness reason appears %d times, want 1", got)
	}
	shared, _ := payloadOf(t, res)["shared_envelope"].(map[string]any)
	fresh, _ := shared["source_freshness"].(map[string]any)
	if fresh == nil || !strings.Contains(fresh["reason"].(string), sharedFreshness) {
		t.Fatalf("shared_envelope lacks source_freshness: %v", shared)
	}
	if _, hoisted := shared["walk_incomplete"]; hoisted {
		t.Errorf("walk_incomplete differs between envelopes and must not be hoisted")
	}
	if got := strings.Count(res.Text, "walk_incomplete"); got != 5 {
		t.Errorf("walk_incomplete fields = %d, want the 5 the producer wrote kept", got)
	}
}

func TestHoistKeepsWhatCannotBeRestored(t *testing.T) {
	cases := []struct {
		name, in string
		want     map[string]bool // field -> hoisted
	}{
		{"identical everywhere",
			`{"a":{"truncated":false,"walk_incomplete":"x"},"b":{"truncated":false,"walk_incomplete":"x"}}`,
			map[string]bool{"walk_incomplete": true}},
		{"one differs",
			`{"a":{"truncated":false,"walk_incomplete":"x"},"b":{"truncated":false,"walk_incomplete":"y"}}`,
			map[string]bool{"walk_incomplete": false}},
		{"an envelope that completed has none",
			`{"a":{"truncated":false,"walk_incomplete":"x"},"b":{"truncated":false,"walk_incomplete":"x"},"c":{"truncated":false}}`,
			map[string]bool{"walk_incomplete": false}},
		{"a single occurrence",
			`{"a":{"truncated":false,"source_freshness":{"fresh":null}}}`,
			map[string]bool{"source_freshness": false}},
	}
	for _, tc := range cases {
		out, shared, err := hoistSharedEnvelopeFields([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for field, hoisted := range tc.want {
			if _, got := shared[field]; got != hoisted {
				t.Errorf("%s: hoisted(%s) = %v, want %v", tc.name, field, got, hoisted)
			}
			if !hoisted && !strings.Contains(string(out), field) {
				t.Errorf("%s: %s was dropped though it was not hoisted: %s", tc.name, field, out)
			}
		}
		var back any
		if err := json.Unmarshal(out, &back); err != nil {
			t.Errorf("%s: output is not JSON: %v", tc.name, err)
		}
	}
}
