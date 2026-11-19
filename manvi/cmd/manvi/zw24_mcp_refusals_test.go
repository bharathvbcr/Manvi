package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/hygiene"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// writeMCPDeclarations puts the declarations the refusal tests need in a repo: a
// server named twice (the second is refused), an empty name, and one fine server.
func writeMCPDeclarations(t *testing.T, root string) {
	t.Helper()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mcp.json", `{"mcpServers":{"dup":{"command":"a"},"":{"command":"c"},"fine":{"command":"d"}}}`)
	write(".mcp.json", `{"mcpServers":{"dup":{"command":"b"}}}`)
}

// captureStderr runs f with os.Stderr redirected, which is where harness boot
// reports what it declined to do.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	func() {
		defer func() { os.Stderr = saved }()
		f()
	}()
	_ = w.Close()
	return <-done
}

// Manager.Skipped had no production reader: a duplicate server name and an empty
// one were refused and nobody was told.
func TestHarnessBootTellsTheOperatorWhatMCPRefused(t *testing.T) {
	root := t.TempDir()
	t.Chdir(t.TempDir())
	writeMCPDeclarations(t, root)
	var core *harnessCore
	stderr := captureStderr(t, func() {
		var err error
		if core, err = bootHarnessCore(newTestRegistry(t), root); err != nil {
			t.Errorf("boot: %v", err)
		}
	})
	if core != nil {
		t.Cleanup(func() { _ = core.plugins.Close() })
	}
	if n := strings.Count(stderr, "manvi: mcp: not offered:"); n != 2 {
		t.Fatalf("boot printed %d refusal notices, want 2 (the duplicate and the empty name):\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "already declared") {
		t.Errorf("the duplicate-name refusal does not say so:\n%s", stderr)
	}
	if strings.Contains(stderr, "more declarations") {
		t.Errorf("a count of omitted refusals was printed when none were omitted:\n%s", stderr)
	}
}

// Harness boot runs once per tool surface in a process. The same refusals are not
// printed again by the next one.
func TestAMCPRefusalIsReportedOncePerProcess(t *testing.T) {
	root := t.TempDir()
	t.Chdir(t.TempDir())
	writeMCPDeclarations(t, root)
	boot := func() string {
		var core *harnessCore
		out := captureStderr(t, func() {
			var err error
			if core, err = bootHarnessCore(newTestRegistry(t), root); err != nil {
				t.Errorf("boot: %v", err)
			}
		})
		if core != nil {
			t.Cleanup(func() { _ = core.plugins.Close() })
		}
		return out
	}
	if first := boot(); strings.Count(first, "not offered:") != 2 {
		t.Fatalf("the first surface did not report both refusals:\n%s", first)
	}
	if second := boot(); second != "" {
		t.Fatalf("a second surface reported the same refusals again:\n%s", second)
	}
}

// Bounded: a repository that declares a hundred bad servers does not print a
// hundred lines, and says how many it left out.
func TestBootRefusalNoticesAreBoundedAndCounted(t *testing.T) {
	skipped := make([]string, 0, 20)
	for i := range 20 {
		skipped = append(skipped, string(rune('a'+i)))
	}
	root := t.TempDir()
	body := strings.Builder{}
	body.WriteString(`{"mcpServers":{`)
	for i, name := range skipped {
		if i > 0 {
			body.WriteString(",")
		}
		body.WriteString(`"` + name + ` ":{"command":"x","env":{"1 bad":"v"}}`)
	}
	body.WriteString("}}")
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	var core *harnessCore
	stderr := captureStderr(t, func() {
		var err error
		if core, err = bootHarnessCore(newTestRegistry(t), root); err != nil {
			t.Errorf("boot: %v", err)
		}
	})
	if core != nil {
		t.Cleanup(func() { _ = core.plugins.Close() })
	}
	if n := strings.Count(stderr, "manvi: mcp: not offered:"); n != bootRefusalNotices {
		t.Fatalf("boot printed %d notices, want the bound %d:\n%s", n, bootRefusalNotices, stderr)
	}
	if !strings.Contains(stderr, "+12 more declarations not offered") {
		t.Fatalf("the omitted count is not said:\n%s", stderr)
	}
}

// A hostile declaration file name cannot repaint the operator's terminal through
// the notice or the doctor row.
func TestRefusalNoticesAreEscaped(t *testing.T) {
	hostile := mcp.SkippedManifest{Path: "x\x1b[2J\u202egpj", Reason: "bad\nforged line\u00ad"}
	lines, more := mcpRefusalLines([]mcp.SkippedManifest{hostile, hostile, hostile}, 2)
	if len(lines) != 2 || more != 1 {
		t.Fatalf("lines %d more %d, want 2 and 1", len(lines), more)
	}
	for _, l := range lines {
		for _, r := range l {
			if hygiene.Invisible(r) || r == '\n' {
				t.Fatalf("a live %U reached the notice: %q", r, l)
			}
		}
	}
}

func TestDoctorListsEveryMCPRefusalWithItsReason(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	defer setProjectRootForTest(root)()
	writeMCPDeclarations(t, root)
	var out bytes.Buffer
	if err := doctor(&out, newTestRegistry(t)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "2 declaration(s) not offered") {
		t.Fatalf("doctor does not count the refusals:\n%s", text)
	}
	if n := strings.Count(text, "NOT OFFERED"); n != 2 {
		t.Fatalf("doctor lists %d refusals, want 2:\n%s", n, text)
	}
	if !strings.Contains(text, "already declared") {
		t.Errorf("the duplicate-name reason is missing:\n%s", text)
	}
}
