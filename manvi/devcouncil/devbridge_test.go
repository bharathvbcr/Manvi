package devcouncil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bridge is tested against a fake CLI, not the real one: what this tool
// owns is discovery, argument assembly, JSON passthrough and exit-code
// honesty. Whether `devcouncil status --json` tells the truth about a project
// is the incumbent's test suite's problem.

// writeFakeCLI installs a script that records its arguments and prints a
// canned payload, returning the path to hand to MANVI_DEVCOUNCIL_BINARY.
func writeFakeCLI(t *testing.T, dir, output string, exitCode int) string {
	t.Helper()
	path := filepath.Join(dir, "fake-devcouncil")
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args.txt") + "\n" +
		"cat <<'EOF'\n" + output + "\nEOF\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDevInspectDiscoversAndPassesThroughJSON(t *testing.T) {
	f := newFixture(t)
	fake := writeFakeCLI(t, t.TempDir(), `{"phase": "NEW"}`, 0)
	t.Setenv(devInspectEnvBinary, fake)

	res := f.call("devcouncil_dev_inspect", map[string]any{})
	if res.IsError {
		t.Fatalf("dev_inspect failed: %s", res.Text)
	}
	var envelope struct {
		Binary     string         `json:"binary"`
		Section    string         `json:"section"`
		ExitCode   int            `json:"exit_code"`
		Devcouncil map[string]any `json:"devcouncil"`
	}
	if err := json.Unmarshal([]byte(res.Text), &envelope); err != nil {
		t.Fatalf("unparseable envelope: %v (%q)", err, res.Text)
	}
	if envelope.Binary != fake || envelope.Section != "status" || envelope.ExitCode != 0 {
		t.Errorf("envelope = %v", res.Text)
	}
	if envelope.Devcouncil["phase"] != "NEW" {
		t.Errorf("payload was not passed through: %q", res.Text)
	}

	// The CLI was invoked with the section, machine-readable output, and this
	// repository as its project root — the three things every consumer of the
	// result depends on.
	rawArgs, err := os.ReadFile(filepath.Join(filepath.Dir(fake), "args.txt"))
	if err != nil {
		t.Fatalf("the fake CLI was never run: %v", err)
	}
	for _, want := range []string{"--json", "--project-root", f.root} {
		if !strings.Contains(string(rawArgs), want) {
			t.Errorf("invocation %q is missing %q", rawArgs, want)
		}
	}
}

func TestDevInspectScopesGapsAndForcesDeterministicCheck(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	fake := writeFakeCLI(t, dir, `{}`, 0)
	t.Setenv(devInspectEnvBinary, fake)

	f.call("devcouncil_dev_inspect", map[string]any{"section": "gaps", "task_id": "TASK-9"})
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(args), "--task-id TASK-9") {
		t.Errorf("gap scoping not passed through: %q", args)
	}

	os.Remove(filepath.Join(dir, "args.txt"))
	f.call("devcouncil_dev_inspect", map[string]any{"section": "check"})
	args, _ = os.ReadFile(filepath.Join(dir, "args.txt"))
	// The LLM audit costs money and needs keys; the tool runs the evidence
	// gate instead, and does so visibly in its argv.
	if !strings.Contains(string(args), "--verify") {
		t.Errorf("check ran without --verify: %q", args)
	}
}

// TestDevInspectSurfacesFailureWithoutDiscardingPayload covers the honesty
// rule: a non-zero exit from the CLI is an error result whose payload still
// carries whatever the CLI managed to say.
func TestDevInspectSurfacesFailureWithoutDiscardingPayload(t *testing.T) {
	f := newFixture(t)
	t.Setenv(devInspectEnvBinary,
		writeFakeCLI(t, t.TempDir(), `{"error": "no project"}`, 3))

	res := f.call("devcouncil_dev_inspect", map[string]any{"section": "status"})
	if !res.IsError {
		t.Fatalf("exit code 3 reported success: %s", res.Text)
	}
	var envelope struct {
		ExitCode   int             `json:"exit_code"`
		Error      string          `json:"error"`
		Devcouncil *map[string]any `json:"devcouncil"`
	}
	if err := json.Unmarshal([]byte(res.Text), &envelope); err != nil {
		t.Fatalf("unparseable failure: %v (%q)", err, res.Text)
	}
	if envelope.ExitCode != 3 || envelope.Error == "" {
		t.Errorf("failure lost the exit code or reason: %s", res.Text)
	}
	if envelope.Devcouncil == nil {
		t.Error("the CLI's partial payload was discarded")
	}
}

// TestDevInspectNamesTheFixWhenTheCLIMissing pins the unavailable shape: no
// binary means an error naming the override variable, never an empty success.
func TestDevInspectNamesTheFixWhenTheCLIMissing(t *testing.T) {
	f := newFixture(t)
	t.Setenv("PATH", "")
	t.Setenv(devInspectEnvBinary, "/nonexistent/devcouncil")

	res := f.call("devcouncil_dev_inspect", map[string]any{})
	if !res.IsError {
		t.Fatalf("a missing CLI reported success: %s", res.Text)
	}
	if !strings.Contains(res.Text, devInspectEnvBinary) && !strings.Contains(res.Text, "could not be determined") {
		t.Errorf("refusal names neither the env var nor its nature: %s", res.Text)
	}
}

func TestDevInspectRejectsUnknownSection(t *testing.T) {
	f := newFixture(t)
	if res := f.call("devcouncil_dev_inspect", map[string]string{"section": "deploy"}); !res.IsError {
		t.Fatalf("unknown section accepted: %s", res.Text)
	}
}

// TestDevInspectLabelsNonJSONOutput covers the version-mismatch case: prose
// comes back labelled as raw_output and degraded rather than parsed as if it
// were structure.
func TestDevInspectLabelsNonJSONOutput(t *testing.T) {
	f := newFixture(t)
	t.Setenv(devInspectEnvBinary, writeFakeCLI(t, t.TempDir(), "Traceback (most recent call last)", 1))

	res := f.call("devcouncil_dev_inspect", map[string]any{})
	var envelope struct {
		RawOutput  string   `json:"raw_output"`
		Degraded   []string `json:"degraded"`
		Devcouncil any      `json:"devcouncil"`
	}
	if err := json.Unmarshal([]byte(res.Text), &envelope); err != nil {
		t.Fatalf("unparseable: %v (%q)", err, res.Text)
	}
	if envelope.RawOutput == "" || envelope.Devcouncil != nil {
		t.Errorf("prose was not labelled as raw: %s", res.Text)
	}
	if len(envelope.Degraded) == 0 {
		t.Error("non-JSON output was not reported as degraded")
	}
}
