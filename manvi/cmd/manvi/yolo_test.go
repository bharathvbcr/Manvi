package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"manvi/flags"
)

func TestTakeYoloStripsTheOptionFromEitherSide(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
		yolo bool
	}{
		{"before the subcommand", []string{"--yolo", "check", "src/a.go"}, []string{"check", "src/a.go"}, true},
		{"after the subcommand", []string{"check", "--yolo", "src/a.go"}, []string{"check", "src/a.go"}, true},
		{"absent", []string{"check", "src/a.go"}, []string{"check", "src/a.go"}, false},
		{"alone", []string{"--yolo"}, []string{}, true},
		{"repeated", []string{"--yolo", "doctor", "--yolo"}, []string{"doctor"}, true},
		// Only the exact option. A path or value that merely starts with it is
		// an argument, and eating it would change what the command operates on.
		{"not a prefix match", []string{"check", "--yolonot"}, []string{"check", "--yolonot"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, yolo := takeYolo(tc.args)
			if yolo != tc.yolo {
				t.Fatalf("yolo = %v, want %v", yolo, tc.yolo)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestYoloOptionMovesThePostureAndSaysSo runs the real dispatch, because the
// point of the option is not that a bool was parsed — it is that the posture
// the rest of the process reads has moved, and that `manvi flags` reports it
// as a safety setting an operator deliberately relaxed.
func TestYoloOptionMovesThePostureAndSaysSo(t *testing.T) {
	// Every invocation scaffolds the directory it runs in, so this one runs
	// somewhere disposable rather than in the source tree.
	t.Chdir(t.TempDir())

	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"--yolo", "flags"}); err != nil {
		t.Fatal(err)
	}
	line := findFlagLine(t, out.String(), flags.HarnessPosture)
	if !strings.Contains(line, flags.PostureYolo) {
		t.Fatalf("flags line = %q, want the yolo posture", line)
	}
	if !strings.Contains(line, string(flags.OriginOverride)) {
		t.Fatalf("flags line = %q, want the origin to name the command line", line)
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "!") {
		t.Fatalf("flags line = %q, want it marked as a safety flag", line)
	}

	var without bytes.Buffer
	if err := run(&without, &notes, []string{"flags"}); err != nil {
		t.Fatal(err)
	}
	if line := findFlagLine(t, without.String(), flags.HarnessPosture); !strings.Contains(line, flags.PostureDev) {
		t.Fatalf("flags line = %q, want the shipped dev posture without --yolo", line)
	}
}

func findFlagLine(t *testing.T, output, key string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, key) {
			return line
		}
	}
	t.Fatalf("no line for %q in:\n%s", key, output)
	return ""
}

// TestDoctorPrintsThePostureNotice: doctor is where an operator checks what the
// harness will do. A posture that relaxed the gates and printed only its name
// would leave "yolo" meaning whatever the reader assumed it meant.
func TestDoctorPrintsThePostureNotice(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Set(flags.Human, flags.HarnessPosture, flags.PostureYolo); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// doctor probes the store and the verifier, and reports them unavailable
	// rather than failing; the posture block above that is what this asserts.
	if err := doctor(&out, reg); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(text, "posture yolo (override)") {
		t.Fatalf("doctor output does not report the posture and its origin:\n%s", out.String())
	}
	if !strings.Contains(text, flags.DescribePosture(flags.PostureYolo).Notice) {
		t.Fatalf("doctor output does not carry the yolo notice:\n%s", out.String())
	}
	if !strings.Contains(text, "WEAKENED") {
		t.Fatalf("doctor output does not list the run as weakened:\n%s", out.String())
	}
	// The gates it resolves are the ones the gate will actually run.
	if !strings.Contains(text, "file gate off") || !strings.Contains(text, "command gate off") {
		t.Fatalf("doctor output does not report both gates off:\n%s", out.String())
	}
	// And the hard rules are reported resolved, not raw: the flag still says
	// true, so a doctor reading the flag would print "on" for rules that are
	// not running.
	if !strings.Contains(text, "hard rules off ("+flags.HarnessPosture+"="+flags.PostureYolo) {
		t.Fatalf("doctor output does not report the hard rules off, or does not name the posture:\n%s", out.String())
	}
	if !strings.Contains(text, "not enforced") {
		t.Fatalf("doctor output does not spell out what stopped enforcing:\n%s", out.String())
	}
}

func TestWrapNoticeKeepsWholeWordsAndDropsEmpty(t *testing.T) {
	if got := wrapNotice("", 20); len(got) != 0 {
		t.Fatalf("wrapNotice(\"\") = %q, want no lines", got)
	}
	lines := wrapNotice(flags.DescribePosture(flags.PostureYolo).Notice, 40)
	if len(lines) < 2 {
		t.Fatalf("lines = %q, want the notice wrapped", lines)
	}
	for _, l := range lines {
		if len(l) > 40 {
			t.Fatalf("line %q exceeds the width", l)
		}
	}
	if strings.Join(lines, " ") != flags.DescribePosture(flags.PostureYolo).Notice {
		t.Fatalf("wrapping changed the text: %q", strings.Join(lines, " "))
	}
}

// TestEveryCommandPreparesTheRepository covers the promise the usage text
// makes: it is the command the operator typed that scaffolds, not a separate
// init they have to remember. 'flags' is the least repository-shaped command
// there is, which is the point — if that one prepares the directory, all of
// them do.
func TestEveryCommandPreparesTheRepository(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"flags"}); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory was not created: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf(".gitignore was not written: %v", err)
	}
	if !strings.Contains(string(ignore), ".devcouncil/*") {
		t.Fatalf(".gitignore does not keep the harness state out of a commit:\n%s", ignore)
	}

	// What it changed is said, and said on the stream that is not the answer:
	// 'manvi tool --json' is piped, and this line must never land in it.
	if !strings.Contains(notes.String(), ".gitignore") {
		t.Fatalf("the scaffolding was silent about what it wrote:\n%s", notes.String())
	}
	if strings.Contains(out.String(), ".gitignore") {
		t.Fatalf("the scaffolding report landed on stdout:\n%s", out.String())
	}

	// And the second command through says nothing, because it changed nothing.
	var second, secondNotes bytes.Buffer
	if err := run(&second, &secondNotes, []string{"flags"}); err != nil {
		t.Fatal(err)
	}
	if secondNotes.Len() != 0 {
		t.Fatalf("a run that changed nothing still reported:\n%s", secondNotes.String())
	}
}

// TestInitCanBeDeclined: a repository the operator does not want written to is
// a repository the harness still runs in.
func TestInitCanBeDeclined(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(flags.EnvKey(flags.HarnessInitEnabled), "false")

	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"flags"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".devcouncil", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written with the setting off (stat error: %v)", name, err)
		}
	}
	if notes.Len() != 0 {
		t.Fatalf("nothing was scaffolded but something was reported:\n%s", notes.String())
	}
}

func TestCheckCommandAndAllow(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	var out, notes bytes.Buffer
	// Check orientation command: should allow
	if err := run(&out, &notes, []string{"check", "--cmd", "git status"}); err != nil {
		t.Fatalf("check --cmd git status: %v", err)
	}
	if !strings.Contains(out.String(), "ALLOW") {
		t.Fatalf("expected ALLOW for git status, got: %s", out.String())
	}

	out.Reset()
	// Check force push: should deny (git safety), and say so in its status.
	//
	// This assertion used to be `err != nil -> fatal`, which is the defect it
	// was written before: a refusal returned nil, so `manvi check` exited 0 on
	// every block. Git safety is a hard rule, so the status is the one that
	// says no grant will clear it.
	err := run(&out, &notes, []string{"check", "--cmd", "git push -f origin main"})
	if !errors.Is(err, errCheckHardBlocked) {
		t.Fatalf("check --cmd force push: got %v, want errCheckHardBlocked", err)
	}
	if !strings.Contains(out.String(), "DENY") {
		t.Fatalf("expected DENY for force push, got: %s", out.String())
	}
}

// TestDoctorDoesNotAttributeTheIndexsCountsToTheArtifact.
//
// doctor read the index with mc.Status and printed those counts beside
// graphArtifactPath(), so its one dev-map line asserted that the code graph
// holds a number of symbols that came from somewhere else. Those two are
// separate files with separate lifetimes: while this repository's index stood
// at generation 4 with 4,249 symbols and the artifact carried generation 2 with
// 2,713, doctor stated the artifact held 4,249.
//
// It is the same conflation `manvi map status` was printing, in the command an
// operator opens first to find out what is wrong.
func TestDoctorDoesNotAttributeTheIndexsCountsToTheArtifact(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	graph := filepath.Join(dir, "code_graph.json")
	// An artifact with two nodes, stamped from generation 2.
	doc := `{"schema_version":2,"meta":{"devmap_rust":{"generation_id":2,"analysis_status":"ok"}},
"nodes":[{"id":"a/x.go","kind":"file","path":"a/x.go","area":"a"},
{"id":"b/y.go","kind":"file","path":"b/y.go","area":"b"}],
"edges":[{"source":"a/x.go","target":"b/y.go","kind":"calls","confidence":"extracted"}]}`
	if err := os.WriteFile(graph, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANVI_GRAPH", graph)

	var out bytes.Buffer
	if err := doctor(&out, reg); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(out.String()), " ")

	// Whatever it says about this file, it must not report a symbol count the
	// file does not have. The index here is whatever the machine happens to
	// hold; the artifact holds two nodes.
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, graph) {
			continue
		}
		if strings.Contains(line, "symbols") && !strings.Contains(line, "2 symbols") {
			t.Fatalf("doctor attributes a count to %s that is not its own: %q", graph, line)
		}
	}
	if !strings.Contains(text, "dev map") {
		t.Fatalf("doctor says nothing at all about the dev map:\n%s", out.String())
	}
}
