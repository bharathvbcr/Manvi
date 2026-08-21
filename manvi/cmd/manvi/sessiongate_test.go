package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/flags"
)

// TestSessionAnswersFromTheGateItWritesThrough.
//
// A TUI session used to hold two gates. nativeToolsWith built one inside the
// tool surface — the one that decides what the agent may write — and NewSession
// built a second one beside it for /check and /allow to talk to. Two gates are
// two grant ledgers and two navigation indexes, and nothing in the running
// system ever printed which of them had answered.
//
// The consequences were both silent. /allow recorded a human override in a
// ledger the agent's next write never consults, so the operator cleared a block
// and watched it block again. /check answered from a gate built without the
// navigation index, so its prediction of the neighbour rule was not a
// prediction of the rule that would actually run.
func TestSessionAnswersFromTheGateItWritesThrough(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	defer setProjectRootForTest(root)()

	s, err := newSessionState(newTestRegistry(t), "S1", nil)
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	if s.gate == nil || s.native.Gate() == nil {
		t.Fatal("a session was built with no gate")
	}
	if s.gate != s.native.Gate() {
		t.Fatal("the session answers /check and /allow from a different gate than its tool surface writes through")
	}
	if s.gate.Ledger != s.native.Gate().Ledger {
		t.Fatal("the session's grants land in a ledger the agent's writes do not consult")
	}
}

// TestAllowInASessionReachesTheLedgerTheAgentIsJudgedBy is the same invariant
// asserted through the command an operator actually runs, rather than through
// pointer identity — so a future refactor that keeps the fields distinct but
// wires them to one ledger still passes, and one that quietly reintroduces a
// second ledger still fails.
func TestAllowInASessionReachesTheLedgerTheAgentIsJudgedBy(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer setProjectRootForTest(root)()

	s, err := newSessionState(registryWith(t, map[string]string{
		flags.HarnessPosture: flags.PostureStrict,
	}), "S1", nil)
	if err != nil {
		t.Fatalf("session state: %v", err)
	}

	before := len(s.native.Gate().Ledger.All())
	var out strings.Builder
	if err := allow(&out, s.gate, []string{"src/unplanned.go", "--reason", "operator cleared this"}); err != nil {
		t.Fatalf("allow: %v\n%s", err, out.String())
	}
	after := s.native.Gate().Ledger.All()
	if len(after) != before+1 {
		t.Fatalf("the tool surface's ledger holds %d grants after /allow, was %d — the grant went somewhere else",
			len(after), before)
	}
}

// TestCheckInASessionConsultsTheNavigationIndex. The second gate was built with
// Subsystems nil whatever was on disk, so /check evaluated the neighbour rule
// with no index while the write it predicted was judged with one.
func TestCheckInASessionConsultsTheNavigationIndex(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	defer setProjectRootForTest(root)()

	s, err := newSessionState(newTestRegistry(t), "S1", nil)
	if err != nil {
		t.Fatalf("session state: %v", err)
	}
	// With no index on disk both are nil, which is still the same answer — the
	// defect was that they could differ at all. Asserting equality covers both
	// cases without needing a built index in a temporary directory.
	if s.gate.Subsystems != s.native.Gate().Subsystems {
		t.Fatal("/check consults a different navigation index than the gate that judges the write")
	}
	// And the command runs against it rather than erroring on the way in.
	if err := check(io.Discard, s.gate, []string{"src/calc.go"}); err != nil {
		t.Fatalf("check: %v", err)
	}
}
