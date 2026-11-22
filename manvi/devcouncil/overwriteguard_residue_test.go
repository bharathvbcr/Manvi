package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file whose shown text was redacted is not a file the agent read in full:
// the digest would be of the raw bytes while the model saw placeholders, and a
// full rewrite would then replace the credential with the placeholder. The
// overwrite guard must refuse until the agent changes the file by patch.
func TestRedactedReadDoesNotCountAsReadInFull(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	const secret = "sk-live-0123456789abcdef"
	f.pipe.SetScrubber(func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") })
	path := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package calc\nconst key = \""+secret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	read := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"})
	if read.IsError || strings.Contains(read.Text, secret) || !strings.Contains(read.Text, "[REDACTED]") {
		t.Fatalf("read did not return the redacted text: error=%v %q", read.IsError, read.Text)
	}
	if !strings.Contains(read.Text, "not counted as read in full") {
		t.Errorf("the read does not tell the agent its view was redacted:\n%s", read.Text)
	}

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\nconst key = \"[REDACTED]\"\n"})
	if !res.IsError || !strings.Contains(res.Text, "has not been read in full") {
		t.Fatalf("overwrite of a file whose shown text was redacted was allowed: error=%v %q", res.IsError, res.Text)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- a path under the test's own temp root
	if err != nil || !strings.Contains(string(got), secret) {
		t.Fatalf("the credential was dropped from the file: %q %v", got, err)
	}
}

// A file read in full before its credential was watched keeps its mark unless
// a later redacted read drops it: the scrubber's watched set grows mid-session,
// so the second read says "write_file will refuse" and that must be true.
func TestLaterRedactedReadDropsAnEarlierReadInFullMark(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	const secret = "sk-live-0123456789abcdef"
	path := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package calc\nconst key = \""+secret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"}); r.IsError {
		t.Fatal(r.Text)
	}
	f.pipe.SetScrubber(func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") })
	read := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"})
	if !strings.Contains(read.Text, "will refuse to overwrite it") {
		t.Fatalf("no redaction note: %q", read.Text)
	}
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\nconst key = \"[REDACTED]\"\n"})
	if !res.IsError {
		t.Errorf("the read said write_file will refuse, but the overwrite went through")
	}
	got, err := os.ReadFile(path) // #nosec G304 -- a path under the test's own temp root
	if err != nil || !strings.Contains(string(got), secret) {
		t.Errorf("the credential was dropped from the file: %q %v", got, err)
	}
}

// A read with nothing to redact still unlocks the overwrite.
func TestUnredactedReadStillCountsAsReadInFull(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	f.pipe.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "sk-live-0123456789abcdef", "[REDACTED]") })
	path := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package calc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"}); read.IsError {
		t.Fatal(read.Text)
	}
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n// x\n"}); res.IsError {
		t.Fatalf("overwrite after a clean full read was refused: %s", res.Text)
	}
}

// A hung index must not hold a write for longer than writeImpactTimeout plus
// the write's own work, and the result must say the callers are unknown rather
// than read as a clean check. The slack absorbs host load, not a second wait.
func TestWriteDoesNotWaitOnAHungImpactLookup(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, true)
	bin := filepath.Join(s.dir, "devmap")
	script, err := os.ReadFile(bin) // #nosec G304 -- the stub this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	hung := strings.Replace(string(script), "  impact) cat ", "  impact) exec sleep 30; cat ", 1)
	if hung == string(script) {
		t.Fatal("the stub has no impact branch to hang")
	}
	if err := os.WriteFile(bin, []byte(hung), 0o700); err != nil { // #nosec G306 -- a stand-in devmap the test execs must be executable
		t.Fatal(err)
	}

	start := time.Now()
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	elapsed := time.Since(start)
	t.Logf("write with a hung impact lookup returned after %s", elapsed)
	if res.IsError {
		t.Fatalf("write refused: %s", res.Text)
	}
	if elapsed > writeImpactTimeout+5*time.Second {
		t.Errorf("write waited %s on the impact lookup, bound is %s", elapsed, writeImpactTimeout)
	}
	if !strings.Contains(res.Text, "impact check did not run") || !strings.Contains(res.Text, "unknown") {
		t.Errorf("a write whose impact lookup did not finish does not say impact is unknown:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "no callers") {
		t.Errorf("an unfinished lookup read as a clean one:\n%s", res.Text)
	}
}
