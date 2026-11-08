package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDurableAdviceNeverPointsAtARefusedFile: the advice printed after
// 'manvi flags set' must not send an operator-scope key to the repository
// config, which manvi refuses at start.
func TestDurableAdviceNeverPointsAtARefusedFile(t *testing.T) {
	user := filepath.Join(t.TempDir(), "user.yaml")
	t.Setenv(envUserConfig, user)
	repo := configPath()
	for _, key := range []string{"llm.tier.fast.model", "llm.provider.default", harnessSpecKey} {
		advice := durableAdvice(key, "x")
		if strings.Contains(advice, "into "+repo) {
			t.Errorf("%s: advice %q names the repository config %s", key, advice, repo)
		}
		if key != harnessSpecKey && !strings.Contains(advice, user) {
			t.Errorf("%s: advice %q does not name the user config %s", key, advice, user)
		}
	}
	if advice := durableAdvice("harness.posture", "x"); !strings.Contains(advice, "into "+repo) {
		t.Errorf("an ordinary key belongs in the repository config; advice %q", advice)
	}
}
