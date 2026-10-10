package agent

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestValidateEffort(t *testing.T) {
	reasons := llm.Capability{Provider: "p", Model: "m", SupportsReasoning: true, EffortLevels: []string{"low", "high"}}
	plain := llm.Capability{Provider: "p", Model: "plain"}
	for _, tc := range []struct {
		name   string
		cap    llm.Capability
		effort string
		want   string
	}{
		{"empty is always fine", plain, "", ""},
		{"supported level", reasons, "high", ""},
		{"level off the ladder", reasons, "max", "supports effort"},
		{"case matters", reasons, "High", "supports effort"},
		{"non-reasoning model takes none", plain, "low", "does not support reasoning effort"},
		{"reasoning flag without levels", llm.Capability{SupportsReasoning: true}, "low", "does not support reasoning effort"},
	} {
		err := ValidateEffort(tc.cap, tc.effort)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}
