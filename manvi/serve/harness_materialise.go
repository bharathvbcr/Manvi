package serve

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	dc "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// Materialising a harness into a managed checkout.
//
// A managed run is an external CLI over a checkout (see managed.go); Manvi does
// not compose its system prompt, so a spec reaches it the one way the CLI takes
// instructions: as text in the instruction file it reads.
//
// Which file is a fact about the CLI and not a choice here:
//
//   - codex reads AGENTS.md. The adapter starts `codex app-server` over the
//     checkout (codexArgs) and AGENTS.md is the file that CLI documents.
//   - claude reads CLAUDE.md. The adapter launches it with `--setting-sources
//     user`, which measurably leaves the project CLAUDE.md unloaded (see
//     claudeArgs), and managedTurn therefore tells the agent to read the
//     repository's AGENTS.md and CLAUDE.md itself. CLAUDE.md is Claude Code's own
//     file, so it is the one a later launch without that flag would load, and
//     when a harness is materialised managedTurn names it as well.
//
// What is written is one delimited block at the tail of that file. Manvi writes
// it, not the agent, so it goes through the gate with internal=true and
// through the pinned writer: a symbolic link or a file with more than one name
// is refused, not followed. The block is replaced when one is already there and
// the rest of the file is never touched. It is taken out again when the run
// ends, by whatever way, so the checkout's own diff never carries it.
//
// Skills: a spec's skills are a name filter over skills the repository already
// has (see harness.Spec.Skills), so there is nothing to write under
// .agents/skills; a spec that carried skill bodies would have to be written
// there, and this release's spec cannot.

const (
	// harnessBeginMarker and harnessEndMarker open and close the block. They are
	// whole lines, and a section's text may not contain either prefix.
	harnessBeginMarker = "<!-- BEGIN MANVI HARNESS"
	harnessEndMarker   = "<!-- END MANVI HARNESS"
	// maxInstructionFileBytes bounds the file read before and after a run. An
	// instruction file past it is refused rather than truncated.
	maxInstructionFileBytes = 4 << 20
)

// ErrHarnessMaterialise is the typed reason a managed run was aborted because
// its harness could not be written into the checkout, or taken out of it.
const ErrHarnessMaterialise = "E_HARNESS_MATERIALISE"

type harnessMaterialiseError struct{ cause error }

func (e *harnessMaterialiseError) Error() string {
	return ErrHarnessMaterialise + ": " + e.cause.Error()
}
func (e *harnessMaterialiseError) Unwrap() error { return e.cause }

func materialiseFailure(format string, args ...any) error {
	return &harnessMaterialiseError{cause: fmt.Errorf(format, args...)}
}

// harnessInstructionFile is the file provider's CLI reads, repository-relative.
func harnessInstructionFile(provider string) (string, bool) {
	switch provider {
	case "claude":
		return "CLAUDE.md", true
	case "codex":
		return "AGENTS.md", true
	}
	return "", false
}

// harnessFileFor is the file a harness would be written to for an attempt, ""
// when it names none or the provider has no such file.
func harnessFileFor(ref ManagedHarness, provider string) string {
	if ref == (ManagedHarness{}) {
		return ""
	}
	file, _ := harnessInstructionFile(provider)
	return file
}

// withHarnessNote adds, to a managed turn, one paragraph saying where the
// harness block is, so the agent is told to follow it and not left to find it.
// It goes after the turn's first paragraph, the agent's identity and the
// conditions of an unattended turn, and before everything that says what to do.
// A turn for an attempt with no harness (harnessFile "") is returned as it is.
//
// What the paragraph says is what is true of the block: Manvi wrote it for this
// run, it sits between the BEGIN and END markers at the end of the file, and it
// binds the agent as the rest of that file does and no further. It widens
// nothing: the permission mode and the host's approval queue are unchanged.
func withHarnessNote(turn, harnessFile string) string {
	if harnessFile == "" {
		return turn
	}
	note := "Manvi added a harness for this run: a block at the end of " + harnessFile +
		" between the lines beginning \"" + harnessBeginMarker + "\" and \"" + harnessEndMarker +
		"\". Read it and follow it with the rest of that file. It is removed when the run ends; do not edit it."
	head, rest, found := strings.Cut(turn, "\n\n")
	if !found {
		return turn + "\n\n" + note
	}
	return head + "\n\n" + note + "\n\n" + rest
}

// renderHarnessBlock is the block for a spec: markers naming the spec and its
// digest, then each section under its own heading, in digest order. It returns
// "" for a spec with no sections, which has nothing to say to the CLI.
func renderHarnessBlock(spec harness.Spec) (string, error) {
	n := spec.Normalized()
	if len(n.Sections) == 0 {
		return "", nil
	}
	tag := fmt.Sprintf("name=%s digest=%s", spec.Name, spec.Digest())
	var b strings.Builder
	b.WriteString(harnessBeginMarker + " " + tag + " -->\n")
	b.WriteString("<!-- Written by Manvi for this managed run and removed when it ends. -->\n")
	for _, sec := range n.Sections {
		if strings.Contains(sec.Text, harnessBeginMarker) || strings.Contains(sec.Text, harnessEndMarker) {
			return "", materialiseFailure("section %q contains a harness block marker, which would make the block's end ambiguous", sec.Name)
		}
		b.WriteString("\n## " + sec.Name + "\n\n")
		b.WriteString(strings.TrimRight(sec.Text, "\n") + "\n")
	}
	b.WriteString(harnessEndMarker + " " + tag + " -->\n")
	return b.String(), nil
}

// blockSpan locates the harness block in content by whole-line markers. found is
// false when there is none. A begin with no end, an end with no begin, or more
// than one block is an error: the span would be a guess, and the file is not
// ours to guess about.
func blockSpan(content []byte) (start, end int, found bool, err error) {
	begin, endAt := -1, -1
	for off := 0; off < len(content); {
		line := content[off:]
		next := len(content)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], off+i+1
		}
		switch {
		case bytes.HasPrefix(line, []byte(harnessBeginMarker)):
			if begin >= 0 {
				return 0, 0, false, materialiseFailure("the instruction file holds more than one harness block")
			}
			begin = off
		case bytes.HasPrefix(line, []byte(harnessEndMarker)):
			if begin < 0 || endAt >= 0 {
				return 0, 0, false, materialiseFailure("the instruction file holds a harness end marker with no matching begin")
			}
			endAt = next
		}
		off = next
	}
	switch {
	case begin < 0 && endAt < 0:
		return 0, 0, false, nil
	case begin < 0 || endAt < 0:
		return 0, 0, false, materialiseFailure("the instruction file holds a harness begin marker with no end")
	}
	return begin, endAt, true, nil
}

// withoutBlock is content with its harness block removed, and the blank
// separator line materialisation put before it. Content with no block is
// returned as it is.
func withoutBlock(content []byte) ([]byte, bool, error) {
	start, end, found, err := blockSpan(content)
	if err != nil || !found {
		return content, false, err
	}
	head := content[:start]
	if bytes.HasSuffix(head, []byte("\n\n")) {
		head = head[:len(head)-1]
	}
	return append(append([]byte(nil), head...), content[end:]...), true, nil
}

// withBlock is base followed by block as a tail: a newline is added when base
// does not end in one, and a blank line separates a non-empty base from the
// block.
func withBlock(base []byte, block string) []byte {
	out := append([]byte(nil), base...)
	if len(out) > 0 {
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, '\n')
	}
	return append(out, block...)
}

// harnessWriter judges and performs the writes of one materialisation.
type harnessWriter struct {
	root, rel string
}

// write judges the write through the same file gate an agent's write meets, as
// an internal write (Manvi's own, so the restricted-path rung does not apply)
// with the hard rules on, then performs it through the pinned writer. The host
// posture's demotion of a taskless soft denial applies, as on policy.check.file;
// a hard denial is a refusal.
func (w harnessWriter) write(op dc.Operation, data []byte) error {
	gate := policy.FileGate{Root: w.root, HardRules: true}
	d := PostureHost.demote(gate.EvaluateFileChange(w.rel, nil, op, true), "managed harness: Manvi's own write, no task model")
	if d.Action == policy.Deny {
		return materialiseFailure("the write gate refused %s (%s): %s", w.rel, d.Rule, d.Reason)
	}
	if err := devcouncil.WritePinned(w.root, w.rel, data, 0o644); err != nil {
		return materialiseFailure("writing %s: %v", w.rel, err)
	}
	return nil
}

func (w harnessWriter) remove() error {
	gate := policy.FileGate{Root: w.root, HardRules: true}
	d := PostureHost.demote(gate.EvaluateFileChange(w.rel, nil, dc.OpDelete, true), "managed harness: Manvi's own write, no task model")
	if d.Action == policy.Deny {
		return materialiseFailure("the write gate refused removing %s (%s): %s", w.rel, d.Rule, d.Reason)
	}
	if err := devcouncil.RemovePinned(w.root, w.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return materialiseFailure("removing %s: %v", w.rel, err)
	}
	return nil
}

// harnessMaterialisation is one written block and what is needed to take it out.
type harnessMaterialisation struct {
	harnessWriter
	// File is the instruction file the block was written to.
	File string
	// existed and before are the file as it stood, with any stale block of an
	// earlier, unfinished run already taken out of it. written is the file as
	// this materialisation left it.
	existed bool
	before  []byte
	written []byte
}

// materialiseHarness writes spec's block into the provider's instruction file
// under root and returns what restores it. It returns nil, nil when there is
// nothing to write: a spec with no sections has nothing to say to the CLI. A
// provider with no instruction file is refused. A failure leaves the file as it
// found it.
func materialiseHarness(root, provider string, spec harness.Spec) (*harnessMaterialisation, error) {
	rel, ok := harnessInstructionFile(provider)
	if !ok {
		return nil, materialiseFailure("provider %q has no instruction file Manvi knows how to write a harness into", provider)
	}
	block, err := renderHarnessBlock(spec)
	if err != nil || block == "" {
		return nil, err
	}
	m := &harnessMaterialisation{harnessWriter: harnessWriter{root: root, rel: rel}, File: rel}
	current, err := devcouncil.ReadPinned(root, rel, maxInstructionFileBytes)
	switch {
	case err == nil:
		m.existed = true
	case errors.Is(err, fs.ErrNotExist):
		current = nil
	default:
		return nil, materialiseFailure("reading %s: %v", rel, err)
	}
	// A block already there is the residue of a run that never restored: it is
	// replaced, so applying is idempotent, and it is not part of the baseline.
	m.before, _, err = withoutBlock(current)
	if err != nil {
		return nil, err
	}
	m.written = withBlock(m.before, block)
	op := dc.OpModify
	if !m.existed {
		op = dc.OpCreate
	}
	if err := m.write(op, m.written); err != nil {
		return nil, err
	}
	return m, nil
}

// Restore takes the block out. A file the run left exactly as materialisation
// wrote it returns to its earlier bytes, or is removed if it did not exist. A
// file the agent changed keeps every change outside the block: only the block
// is cut, and a file that was not there before and is empty without it is
// removed. A block the agent already removed is not an error.
func (m *harnessMaterialisation) Restore() error {
	if m == nil {
		return nil
	}
	current, err := devcouncil.ReadPinned(m.root, m.rel, maxInstructionFileBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // the agent deleted it; there is no block left to take out
	}
	if err != nil {
		return materialiseFailure("reading %s to restore it: %v", m.rel, err)
	}
	target := m.before
	if !bytes.Equal(current, m.written) {
		var found bool
		if target, found, err = withoutBlock(current); err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	if !m.existed && len(bytes.TrimSpace(target)) == 0 {
		return m.remove()
	}
	return m.write(dc.OpModify, target)
}
