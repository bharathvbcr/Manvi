//go:build unix

package devcouncil

import (
	"os"
	"strings"
	"testing"
)

// G7 adversarial: repeat the setsid helper so a flake or a leaked pid shows up,
// and check every pid the helper left is dead once its subtest's cleanup ran.
func TestAdvSetsidHelperRepeatsWithoutFlakeOrLeak(t *testing.T) {
	const rounds = 12
	g := newHelperGuard(t)
	for i := 0; i < rounds; i++ {
		var grouped, escaped int
		t.Run("round", func(t *testing.T) {
			grouped = startHelperTree(t, g, "parent-samegroup")
			escaped = startHelperTree(t, g, "parent-setsid")
			if !g.until(func() bool { return !processAlive(grouped) }) {
				t.Errorf("grouped descendant %d survived the group kill for %s", grouped, g.bound)
			}
			if !processAlive(escaped) {
				t.Errorf("setsid descendant %d died", escaped)
			}
		})
		// The subtest's Cleanup has run: SIGKILL was sent to both pids.
		if !g.until(func() bool { return !processAlive(escaped) && !processAlive(grouped) }) {
			t.Errorf("round %d leaked pid(s) %d/%d (zombie or not killed) for %s", i, escaped, grouped, g.bound)
		}
	}
}

// The comment claims a survivor that moves a ref between calls "is reported".
// refguard only reports when the moved commits carry a credential
// (TestCleanCommitsBetweenCallsAreNotReported), so the sentence is broader than
// the enforcement. Pinned as a source check so a rewording is noticed.
func TestAdvRunProcCommentNamesTheCredentialCondition(t *testing.T) {
	raw, err := os.ReadFile("subprocess.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	i := strings.Index(text, "refGuard.baseline")
	if i < 0 {
		t.Fatal("comment no longer names refGuard.baseline")
	}
	window := text[i : i+400]
	if !strings.Contains(strings.ToLower(window), "credential") && !strings.Contains(strings.ToLower(window), "leak") {
		t.Error("subprocess.go overstates the ref guard: it does not name the credential condition")
	}
}
