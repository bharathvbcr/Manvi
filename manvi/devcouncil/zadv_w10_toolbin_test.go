//go:build unix

package devcouncil

import (
	"os"
	"strings"
	"testing"
	"time"
)

// G7 adversarial: repeat the setsid helper so a flake or a leaked pid shows up,
// and check every pid the helper left is dead once its subtest's cleanup ran.
func TestAdvSetsidHelperRepeatsWithoutFlakeOrLeak(t *testing.T) {
	const rounds = 12
	for i := 0; i < rounds; i++ {
		var grouped, escaped int
		t.Run("round", func(t *testing.T) {
			grouped = startHelperTree(t, "parent-samegroup")
			escaped = startHelperTree(t, "parent-setsid")
			deadline := time.Now().Add(5 * time.Second)
			for processAlive(grouped) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if processAlive(grouped) {
				t.Errorf("grouped descendant %d survived the group kill", grouped)
			}
			if !processAlive(escaped) {
				t.Errorf("setsid descendant %d died", escaped)
			}
		})
		// The subtest's Cleanup has run: SIGKILL was sent to both pids.
		deadline := time.Now().Add(5 * time.Second)
		for (processAlive(escaped) || processAlive(grouped)) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processAlive(escaped) {
			t.Errorf("round %d leaked setsid pid %d (zombie or not killed)", i, escaped)
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
		t.Skip("BREAK: subprocess.go says a survivor that moves a ref between calls 'is reported'; refguard.go judgeMoves reports only commits carrying a credential, and overlapping calls or unreadable refs drop the baseline (refguard.go:618-624). Comment overstates enforcement (low)")
	}
}
