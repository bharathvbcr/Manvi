package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The digest check in overwriteRefusal and the replace are two steps. A change
// that lands between them must be refused at the replace, not clobbered.
func TestOverwriteRefusesAChangeBetweenTheDigestCheckAndTheReplace(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	target := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package calc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"}); res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	other := "package calc\n\nfunc Added() {}\n"
	fired := 0
	failSeam(t, &beforeReplace, func() {
		fired++
		if err := os.WriteFile(target, []byte(other), 0o600); err != nil {
			t.Error(err)
		}
	})

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n\nfunc Mine() {}\n"})
	if fired != 1 {
		t.Fatalf("the replace seam ran %d times, want 1", fired)
	}
	if !res.IsError || !strings.Contains(res.Text, "changed since") {
		t.Fatalf("a change between the check and the replace was not refused: %+v", res)
	}
	if res.Blocked || res.Rule != "" {
		t.Errorf("the gate did not make this refusal: %+v", res)
	}
	if got, _ := os.ReadFile(target); string(got) != other { // #nosec G304 -- a file under this test's own t.TempDir
		t.Fatalf("the concurrent writer's bytes were clobbered: %q", got)
	}
	if extra := leftovers(t, filepath.Dir(target), "calc.go"); len(extra) != 0 {
		t.Errorf("the refused write left %v behind", extra)
	}
	if len(res.Wrote) != 0 {
		t.Errorf("a refused write claimed %v", res.Wrote)
	}
}

const deletable = "internal/extra.go"

// seedDeletable puts an unplanned file on disk and grants the override its
// deletion needs, as TestDeleteFileLifecycle does.
func seedDeletable(t *testing.T, f *fixture, body string) string {
	t.Helper()
	target := filepath.Join(f.root, deletable)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := f.payload("devcouncil_request_override", map[string]string{
		"path": deletable, "rule": "scope.unplanned", "reason": "delete temporary file"}); out["granted"] != true {
		t.Fatalf("override: %v", out)
	}
	return target
}

// A delete is a write the index has not seen, exactly like a patch.
func TestDeleteIsRecordedAsWrittenForTheStaleNotice(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, false)
	s.set(t, "explore", emptyExplore)
	seedDeletable(t, f, "package calc\n")
	if res := f.call("devcouncil_delete_file", map[string]string{"path": deletable}); res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	res := f.call(codeToolName, map[string]string{"kind": "explore", "query": "Sub"})
	if res.IsError {
		t.Fatalf("explore: %s", res.Text)
	}
	if !strings.Contains(res.Text, "index_notice") || !strings.Contains(res.Text[:strings.Index(res.Text, "\"answer\"")], deletable) {
		t.Fatalf("a graph answer after a delete does not name the deleted path as unseen:\n%.600s", res.Text)
	}
}

// A path that was deleted and comes back is a different file: what the agent
// read before is not what it knows now, even when the bytes match.
func TestDeleteClearsTheOverwriteGuard(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	const body = "package calc\n"
	target := seedDeletable(t, f, body)
	if res := f.call("devcouncil_read_file", map[string]string{"path": deletable}); res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if res := f.call("devcouncil_delete_file", map[string]string{"path": deletable}); res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call("devcouncil_write_file", map[string]string{"path": deletable, "content": "package calc\n// new\n"})
	if !res.IsError || !strings.Contains(res.Text, "has not been read in full") {
		t.Fatalf("an overwrite of a re-created file needed no fresh read: %+v", res)
	}
}

// Whether the read-before-overwrite guard runs is decided by what the write
// really replaces (the pinned target), not by an earlier existence check: a file
// that appears after that check, unread, must not be overwritten.
func TestOverwriteGuardFollowsThePinnedTargetNotTheEarlierExistenceCheck(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	target := filepath.Join(f.root, "src", "late.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	const theirs = "package calc\n\nfunc Theirs() {}\n"
	fired := 0
	failSeam(t, &afterCreatedCheck, func() {
		fired++
		if err := os.WriteFile(target, []byte(theirs), 0o600); err != nil {
			t.Error(err)
		}
	})

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/late.go", "content": "package calc\n"})
	if fired != 1 {
		t.Fatalf("the seam ran %d times, want 1", fired)
	}
	if !res.IsError || !strings.Contains(res.Text, "has not been read in full") {
		t.Fatalf("a file that appeared after the existence check was overwritten unread: %+v", res)
	}
	if got, _ := os.ReadFile(target); string(got) != theirs { // #nosec G304 -- under this test's own t.TempDir
		t.Fatalf("the unread file was overwritten: %q", got)
	}
}
