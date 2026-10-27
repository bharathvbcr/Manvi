package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// The project instructions (AGENTS.md, CLAUDE.md, GEMINI.md) are repository
// text that reaches every turn's system prompt, so they are read the way every
// other repository byte is read: through the contained, gated, pinned reader,
// rooted at the project root and not at the process's working directory. The
// tests here drive the same two calls both faces make (resolveTurnInputs, then
// assemblePromptReported) with the production reader over a real directory.

// realRoot is a temporary directory with symbolic links resolved, as the
// project root is (findProjectRoot evaluates them).
func realRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// repoReader is the reader both faces hand the per-turn resolution: the native
// registry's RepoReader, which is devcouncil.RepoReaderAt over the project root.
func repoReader(root string) func(context.Context) harness.ReadFunc {
	return func(ctx context.Context) harness.ReadFunc { return devcouncil.RepoReaderAt(ctx, root) }
}

// instructionsTurn resolves one turn's inputs and assembles its prompt, the way
// run.go and tui.go do, and returns the prompt text and the assembly's faults.
func instructionsTurn(t *testing.T, provider string, read func(context.Context) harness.ReadFunc) (string, []string) {
	t.Helper()
	reg := specRegistry(t, nil)
	opts := PromptOptions{Provider: provider, TaskToolsOffered: true}
	resolveTurnInputs(context.Background(), reg,
		turnInputs{skills: fakeSkills{}, repoRead: read}, &turnState{}, &opts)
	text, _, faults := assemblePromptReported(reg, opts)
	return text, faults
}

func hasInstructions(text string) bool {
	return strings.Contains(text, "Project instructions (")
}

func faultNaming(faults []string, parts ...string) bool {
	for _, f := range faults {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(f, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// AUDIT 2026-10-26 F1: a symbolic link named AGENTS.md carried a file from
// outside the repository into the system prompt and on to the provider.
func TestProjectInstructionsDoNotFollowALinkOutOfTheRepository(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key = AUDIT-SECRET-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := realRoot(t)
	if err := os.Symlink(secret, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	chdir(t, root)

	for _, provider := range []string{"anthropic", "local"} {
		text, faults := instructionsTurn(t, provider, repoReader(root))
		if strings.Contains(text, "AUDIT-SECRET-VALUE") {
			t.Fatalf("%s: a symlinked AGENTS.md pulled a file from outside the repository into the system prompt", provider)
		}
		if !faultNaming(faults, "AGENTS.md") {
			t.Fatalf("%s: the refusal was silent, so the operator cannot learn why the file is not in the prompt: %v", provider, faults)
		}
	}
}

// AUDIT 2026-10-26 F1b: a FIFO named AGENTS.md parked prompt assembly forever.
func TestProjectInstructionsFIFODoesNotBlockAssembly(t *testing.T) {
	root := realRoot(t)
	mkfifo(t, filepath.Join(root, "AGENTS.md"))
	chdir(t, root)

	type result struct {
		text   string
		faults []string
	}
	done := make(chan result, 1)
	go func() {
		text, faults := instructionsTurn(t, "anthropic", repoReader(root))
		done <- result{text, faults}
	}()
	select {
	case r := <-done:
		if hasInstructions(r.text) {
			t.Fatal("a FIFO was read as the project instructions")
		}
		if !faultNaming(r.faults, "AGENTS.md") {
			t.Fatalf("the refusal was silent: %v", r.faults)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt assembly is still blocked 10s after a FIFO was named AGENTS.md")
	}
}

// A file with a second name is how the contents of one the gate would withhold
// reach a prompt: the reader refuses it, as read_file does.
func TestProjectInstructionsRefuseAFileWithAnotherName(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key = AUDIT-SECRET-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := realRoot(t)
	if err := os.Link(secret, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
	chdir(t, root)

	text, faults := instructionsTurn(t, "anthropic", repoReader(root))
	if strings.Contains(text, "AUDIT-SECRET-VALUE") {
		t.Fatal("a hard link to an outside file reached the system prompt")
	}
	if !faultNaming(faults, "AGENTS.md") {
		t.Fatalf("the refusal was silent: %v", faults)
	}
}

// Whatever the gate withholds is withheld here: the reader's refusal is the
// answer, whatever its reason, and the turn runs without the file.
func TestProjectInstructionsHonourTheReadGate(t *testing.T) {
	read := func(context.Context) harness.ReadFunc {
		return func(rel string, _ int64) ([]byte, error) {
			return nil, fmt.Errorf("%s is withheld by path.secret_read", rel)
		}
	}
	text, faults := instructionsTurn(t, "anthropic", read)
	if hasInstructions(text) {
		t.Fatal("instructions the gate withheld are in the prompt")
	}
	if !faultNaming(faults, "AGENTS.md", "path.secret_read") {
		t.Fatalf("the gate's reason did not reach the operator: %v", faults)
	}
}

// AUDIT 2026-10-26 F1: no size cap when the budget is zero, which is every
// non-local provider.
func TestProjectInstructionsOverTheCapAreRefusedOnEveryProvider(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, "AGENTS.md", "OVERSIZE-MARKER\n"+strings.Repeat("- a rule\n", 500_000))
	chdir(t, root)

	for _, provider := range []string{"anthropic", "local"} {
		text, faults := instructionsTurn(t, provider, repoReader(root))
		if strings.Contains(text, "OVERSIZE-MARKER") {
			t.Fatalf("%s: a 4.5 MB instructions file reached the system prompt", provider)
		}
		if !faultNaming(faults, "AGENTS.md", "exceeds") {
			t.Fatalf("%s: the refusal did not say the file is too large: %v", provider, faults)
		}
	}
}

func TestProjectInstructionsNormalFileIsInThePrompt(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, "AGENTS.md", "- run the full suite before claiming a fix\n")
	chdir(t, root)

	for _, provider := range []string{"anthropic", "local"} {
		text, faults := instructionsTurn(t, provider, repoReader(root))
		if !strings.Contains(text, "Project instructions (AGENTS.md):\n- run the full suite before claiming a fix") {
			t.Fatalf("%s: the instructions are not in the prompt", provider)
		}
		for _, f := range faults {
			if strings.Contains(f, "instructions") {
				t.Fatalf("%s: a healthy file produced a fault: %v", provider, f)
			}
		}
	}
}

// The project root, not the process's working directory, names the file: a
// session started in a subdirectory carries the repository's instructions, and
// a stray AGENTS.md in the subdirectory is not mistaken for them.
func TestProjectInstructionsAreReadFromTheProjectRoot(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, "AGENTS.md", "- ROOT-RULE\n")
	sub := filepath.Join(root, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "AGENTS.md", "- SUBDIRECTORY-RULE\n")
	chdir(t, sub)

	text, _ := instructionsTurn(t, "anthropic", repoReader(root))
	if !strings.Contains(text, "ROOT-RULE") || strings.Contains(text, "SUBDIRECTORY-RULE") {
		t.Fatal("the instructions came from the working directory and not the project root")
	}
}

// A symbolic link is refused even inside the repository, as it is for a skill:
// the pinned reader never follows a final-component link, and a second, laxer
// judgement here would be the place the gate was forgotten. The common layout
// where AGENTS.md links to CLAUDE.md still works, because the real file is the
// next candidate; the refusal is reported once so it is not a silent surprise.
func TestProjectInstructionsLinkInsideTheRepositoryFallsThroughToTheRealFile(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, "CLAUDE.md", "- REAL-RULE\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	chdir(t, root)

	text, faults := instructionsTurn(t, "anthropic", repoReader(root))
	if !strings.Contains(text, "Project instructions (CLAUDE.md):\n- REAL-RULE") {
		t.Fatalf("the real file behind the link was not used: %q", text)
	}
	if strings.Contains(text, "Project instructions (AGENTS.md)") {
		t.Fatal("a symbolic link was read as the instructions file")
	}
	if !faultNaming(faults, "AGENTS.md") {
		t.Fatalf("the refused link was not reported: %v", faults)
	}
}

// Absence is not a fault: a repository with only CLAUDE.md, an empty AGENTS.md,
// or no instructions at all runs as it always has.
func TestProjectInstructionsAbsenceIsNotAFault(t *testing.T) {
	root := realRoot(t)
	chdir(t, root)
	text, faults := instructionsTurn(t, "anthropic", repoReader(root))
	if hasInstructions(text) || faultNaming(faults, "instructions") {
		t.Fatalf("a repository with none produced a section or a fault: %v", faults)
	}

	writeFile(t, root, "AGENTS.md", "  \n\n")
	writeFile(t, root, "GEMINI.md", "- GEMINI-RULE\n")
	text, faults = instructionsTurn(t, "anthropic", repoReader(root))
	if !strings.Contains(text, "Project instructions (GEMINI.md):\n- GEMINI-RULE") {
		t.Fatal("an empty AGENTS.md hid the next candidate")
	}
	if faultNaming(faults, "instructions") {
		t.Fatalf("an empty file is not a fault: %v", faults)
	}
}

// countingReader records every path a turn reads.
type countingReader struct {
	mu    sync.Mutex
	files repoFiles
	reads map[string]int
}

func (c *countingReader) read(context.Context) harness.ReadFunc {
	return func(rel string, _ int64) ([]byte, error) {
		c.mu.Lock()
		c.reads[rel]++
		c.mu.Unlock()
		if v, ok := c.files[rel]; ok {
			return []byte(v), nil
		}
		return nil, fmt.Errorf("reading %s: %w", rel, fs.ErrNotExist)
	}
}

// The instructions are resolved once per turn and carried in the options, not
// re-read by each of the five places that build the source list: the reserved
// names are asked three times, a spec's collision check once more, and assembly
// once. The spec here has a section so every one of those runs.
func TestProjectInstructionsAreReadOncePerTurn(t *testing.T) {
	reg := specRegistry(t, map[string]string{harnessSpecKey: "house"})
	cr := &countingReader{
		files: repoFiles{
			"AGENTS.md":                      "- ONCE\n",
			harness.SpecsDir + "/house.json": specJSON("house", "Prefer small functions."),
		},
		reads: map[string]int{},
	}
	opts := PromptOptions{Provider: "anthropic", TaskToolsOffered: true}
	id := resolveTurnInputs(context.Background(), reg,
		turnInputs{skills: fakeSkills{}, repoRead: cr.read}, &turnState{}, &opts)
	if id.Name != "house" {
		t.Fatalf("the spec was not applied, so the collision check did not run: %+v", id)
	}
	text, _, _ := assemblePromptReported(reg, opts)
	if !strings.Contains(text, "- ONCE") || !strings.Contains(text, "Prefer small functions.") {
		t.Fatal("the turn lost the instructions or the spec")
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		if n := cr.reads[name]; n > 1 {
			t.Fatalf("%s was read %d times in one turn", name, n)
		}
	}
	if cr.reads["AGENTS.md"] != 1 {
		t.Fatalf("AGENTS.md was read %d times, want exactly 1", cr.reads["AGENTS.md"])
	}
}

// A turn that cannot reach the repository's reader at all runs without the
// instructions and says so: it is not a reason to read the working directory.
func TestProjectInstructionsWithoutAReaderAreReportedMissing(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, "AGENTS.md", "- NO-READER-RULE\n")
	chdir(t, root)
	reg := specRegistry(t, nil)
	opts := PromptOptions{Provider: "anthropic", TaskToolsOffered: true}
	resolveTurnInputs(context.Background(), reg, turnInputs{skills: fakeSkills{}}, &turnState{}, &opts)
	text, _, faults := assemblePromptReported(reg, opts)
	if strings.Contains(text, "NO-READER-RULE") {
		t.Fatal("with no reader the working directory was read instead")
	}
	if !faultNaming(faults, "instructions") {
		t.Fatalf("a turn with no reader said nothing: %v", faults)
	}
}

// A cancelled read is not a refusal of the file and not "no file": the turn
// runs without instructions and says why, and no later candidate is tried.
func TestProjectInstructionsCancelledReadIsReported(t *testing.T) {
	read := func(context.Context) harness.ReadFunc {
		return func(rel string, _ int64) ([]byte, error) {
			return nil, fmt.Errorf("reading %s: %w", rel, context.Canceled)
		}
	}
	text, faults := instructionsTurn(t, "anthropic", read)
	if hasInstructions(text) {
		t.Fatal("a cancelled read produced a section")
	}
	if !faultNaming(faults, "instructions", "canceled") {
		t.Fatalf("a cancelled read was not reported as one: %v", faults)
	}
}

// The bound is on the opened file, so a file exactly at the cap is read and one
// byte over is refused with its size.
func TestProjectInstructionsCapBoundary(t *testing.T) {
	root := realRoot(t)
	line := "- rule\n"
	atCap := strings.Repeat(line, maxProjectInstructionBytes/len(line))
	atCap += strings.Repeat("x", maxProjectInstructionBytes-len(atCap))
	writeFile(t, root, "AGENTS.md", atCap)
	src, notices := projectInstructionsSource(devcouncil.RepoReaderAt(context.Background(), root), 0)
	if src == nil || len(notices) != 0 {
		t.Fatalf("a file exactly at the cap was refused: %v", notices)
	}

	writeFile(t, root, "AGENTS.md", atCap+"x")
	src, notices = projectInstructionsSource(devcouncil.RepoReaderAt(context.Background(), root), 0)
	if src != nil || !faultNaming(notices, "AGENTS.md", fmt.Sprint(maxProjectInstructionBytes+1)) {
		t.Fatalf("a file one byte over the cap was not refused with its size: src=%v notices=%v", src, notices)
	}
}

// The source for a non-local provider is built with a zero budget and still
// carries its file, label and trimmed body: the shape the assembler orders.
func TestProjectInstructionsSourceShape(t *testing.T) {
	read := func(rel string, _ int64) ([]byte, error) {
		if rel == "CLAUDE.md" {
			return []byte("\n  - shape\n\n"), nil
		}
		return nil, fs.ErrNotExist
	}
	src, notices := projectInstructionsSource(read, 0)
	if len(notices) != 0 || src == nil {
		t.Fatalf("src=%v notices=%v", src, notices)
	}
	if src.Name() != projectInstructionsName {
		t.Fatalf("name = %q", src.Name())
	}
	secs, err := src.Sections()
	if err != nil || len(secs) != 1 || secs[0].Text != "Project instructions (CLAUDE.md):\n- shape" {
		t.Fatalf("sections = %+v, err = %v", secs, err)
	}
}

// The literal secret-path case: a repository's own .env, linked in under the
// instructions file's name. The name AGENTS.md is innocent, so the gate's name
// rule alone would let it through; the reader's one-name rule is what refuses
// it, and no byte of the secret reaches the prompt or the provider.
func TestProjectInstructionsRefuseADotEnvLinkedUnderTheirName(t *testing.T) {
	root := realRoot(t)
	writeFile(t, root, ".env", "API_TOKEN=DOTENV-SECRET-VALUE\n")
	if err := os.Link(filepath.Join(root, ".env"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
	chdir(t, root)

	for _, provider := range []string{"anthropic", "local"} {
		text, faults := instructionsTurn(t, provider, repoReader(root))
		if strings.Contains(text, "DOTENV-SECRET-VALUE") {
			t.Fatalf("%s: the contents of .env reached the system prompt under the name AGENTS.md", provider)
		}
		if !faultNaming(faults, "AGENTS.md") {
			t.Fatalf("%s: the refusal was silent: %v", provider, faults)
		}
	}
}
