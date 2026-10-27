package serve

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
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
//     when a harness is materialised the turn names it as well.
//
// What is written is one delimited block appended at the tail of that file.
// Manvi writes it, not the agent, so it goes through the gate with internal=true
// and through the pinned writer: a symbolic link or a file with more than one
// name is refused, not followed. The block is taken out again when the run
// ends, by whatever way, so the checkout's own diff never carries it.
//
// What makes the removal safe is that Manvi recognises its own bytes and nothing
// else:
//
//   - Every block carries a fresh 128-bit nonce in its BEGIN and END lines. Text
//     that merely looks like a block, in a repository's docs or in an agent's
//     edit, has no nonce Manvi issued and is ordinary content: never matched,
//     never removed, never a reason to refuse a run.
//   - What was appended is recorded, byte for byte, with the offset it was
//     appended at, before it is written: in memory for the run, and in a small
//     file under the checkout's .devcouncil tool-state directory for a crash. A
//     restore removes spans byte-identical to the recorded bytes and nothing
//     else; it never strips a newline or a marker line by pattern. Everything
//     else in the file, the agent's edits included, stays exactly as it is.
//   - A block the agent edited inside is not Manvi's bytes any more. Restore
//     refuses with a typed reason and leaves the file alone, and the record is
//     kept so the next Prepare on the checkout reports it, once, and clears it.
//     It never guesses where a modified block ends.
//
// Skills: a spec's skills are a name filter over skills the repository already
// has (see harness.Spec.Skills), so there is nothing to write under
// .agents/skills; a spec that carried skill bodies would have to be written
// there, and this release's spec cannot.

const (
	// harnessBeginMarker and harnessEndMarker open and close the block. A
	// section's text may not contain either prefix.
	harnessBeginMarker = "<!-- BEGIN MANVI HARNESS"
	harnessEndMarker   = "<!-- END MANVI HARNESS"
	// maxInstructionFileBytes bounds the file read before and after a run. An
	// instruction file past it is refused rather than truncated.
	maxInstructionFileBytes = 4 << 20
	// harnessPendingFile records the block a run has written until it is taken
	// out. It is under .devcouncil, the tool-state directory every verification
	// diff already leaves out, and not under .devcouncil/harness, where specs are.
	harnessPendingFile = ".devcouncil/managed-harness-pending.json"
	// maxPendingBytes bounds the record: the appended bytes in base64 plus a
	// small envelope.
	maxPendingBytes = maxInstructionFileBytes*2 + 4096
)

// ErrHarnessMaterialise is the typed reason a managed run was aborted because
// its harness could not be written into the checkout, or taken out of it.
const ErrHarnessMaterialise = "E_HARNESS_MATERIALISE"

// errRecordUnusable and errBlockModified are the two restore refusals that leave
// nothing a later restore could do, so recovery clears the record on them: the
// record is not a block Manvi would have written, or the agent edited inside it.
var (
	errRecordUnusable = errors.New("the record of the harness block is not usable")
	errBlockModified  = errors.New("the checkout carries a modified harness block")
)

type harnessMaterialiseError struct {
	cause error
	// mayHaveChanged is set on a failed write that may still have changed the
	// file (devcouncil.WriteMayHaveChanged): the outcome is unknown, not failed.
	mayHaveChanged bool
}

func (e *harnessMaterialiseError) Error() string {
	return ErrHarnessMaterialise + ": " + e.cause.Error()
}
func (e *harnessMaterialiseError) Unwrap() error { return e.cause }

// writeMayHaveChanged says whether err is a failed write whose effect on the
// file is unknown.
func writeMayHaveChanged(err error) bool {
	var typed *harnessMaterialiseError
	return errors.As(err, &typed) && typed.mayHaveChanged
}

func materialiseFailure(format string, args ...any) error {
	return &harnessMaterialiseError{cause: fmt.Errorf(format, args...)}
}

// newHarnessNonce issues the nonce of one block: 128 random bits as hex. A
// variable so a test can fix it.
var newHarnessNonce = func() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

var nonceForm = regexp.MustCompile(`^[0-9a-f]{32}$`)

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

func isInstructionFile(name string) bool { return name == "CLAUDE.md" || name == "AGENTS.md" }

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

// renderHarnessBlock is the block for a spec: BEGIN and END lines naming the
// nonce, the spec and its digest, then each section under its own heading, in
// digest order. It returns "" for a spec with no sections, which has nothing to
// say to the CLI.
func renderHarnessBlock(spec harness.Spec, nonce string) (string, error) {
	n := spec.Normalized()
	if len(n.Sections) == 0 {
		return "", nil
	}
	tag := fmt.Sprintf("nonce=%s name=%s digest=%s", nonce, spec.Name, spec.Digest())
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

// separator is what is put between a file's own content and the block: a
// newline when the content does not end in one, then a blank line. Nothing for
// an empty file. It is part of what is recorded and removed, so a restore never
// has to decide whether a newline was Manvi's.
func separator(before []byte) []byte {
	if len(before) == 0 {
		return nil
	}
	if before[len(before)-1] != '\n' {
		return []byte("\n\n")
	}
	return []byte("\n")
}

// pendingRestore is everything needed to take one block out: written to disk
// before the block is, so a crash leaves a record and never a block with none.
type pendingRestore struct {
	Version int    `json:"version"`
	Nonce   string `json:"nonce"`
	File    string `json:"file"`
	// Existed is whether the file was there before the block was.
	Existed bool `json:"existed"`
	// Offset is where Appended was put, the length of the file's own content.
	Offset int `json:"offset"`
	// Appended is the exact bytes added: the separator and the block.
	Appended []byte `json:"appended"`
}

// valid says whether a record, which sits in a directory an agent under a
// relaxed posture can write, can be acted on. A record that could name any
// path, or any bytes, would be an instruction to delete; this one can name two
// files and a span that is framed by its own nonce'd lines.
func (p pendingRestore) valid() error {
	switch {
	case p.Version != 1:
		return fmt.Errorf("record version %d is not one this build reads", p.Version)
	case !nonceForm.MatchString(p.Nonce):
		return errors.New("the nonce is not 128 bits of lowercase hex")
	case !isInstructionFile(p.File):
		return fmt.Errorf("%q is not an instruction file Manvi writes a harness into", p.File)
	case p.Offset < 0 || p.Offset > maxInstructionFileBytes:
		return errors.New("the offset is out of range")
	case len(p.Appended) == 0 || len(p.Appended) > maxInstructionFileBytes:
		return errors.New("the recorded bytes are empty or over the limit")
	}
	body := bytes.TrimLeft(p.Appended, "\n")
	if len(p.Appended)-len(body) > 2 {
		return errors.New("the recorded bytes do not start as a block does")
	}
	tag := "nonce=" + p.Nonce + " "
	begin, end := []byte(harnessBeginMarker+" "+tag), []byte(harnessEndMarker+" "+tag)
	if !bytes.HasPrefix(body, begin) || bytes.Count(p.Appended, begin) != 1 || bytes.Count(p.Appended, end) != 1 {
		return errors.New("the recorded bytes are not one block carrying this nonce")
	}
	lastLine := body[bytes.LastIndex(body[:len(body)-1], []byte("\n"))+1:]
	if !bytes.HasPrefix(lastLine, end) || !bytes.HasSuffix(lastLine, []byte("-->\n")) {
		return errors.New("the recorded bytes do not end with the block's END line")
	}
	return nil
}

// fitsRestoreBound says whether the file with this record's block in it is one
// the restore can read back. Offset+len(Appended) is the file's size once the
// block is written, and every read of an instruction file (the one before the
// write and the one that takes the block out) is under maxInstructionFileBytes,
// so a block that would take the file past it could be written and never
// removed. It is checked before anything is written. valid() does not check it:
// it bounds the two parts one at a time, and a record an earlier build wrote
// under that looser rule must still be read, and kept when it cannot be acted on.
func (p pendingRestore) fitsRestoreBound() error {
	if size := p.Offset + len(p.Appended); size > maxInstructionFileBytes {
		return fmt.Errorf("%s would be %d bytes with the block in it, over the %d-byte limit on what Manvi reads back to take the block out again (the file is %d bytes, the block %d)",
			p.File, size, maxInstructionFileBytes, p.Offset, len(p.Appended))
	}
	return nil
}

// readInstructionFile is the one read of an instruction file, before the block
// is written and when it is taken out, so both are under the same bound.
func readInstructionFile(root, rel string) ([]byte, error) {
	return devcouncil.ReadPinned(root, rel, maxInstructionFileBytes)
}

// harnessWriter judges and performs the writes of one materialisation.
type harnessWriter struct {
	root string
}

// write judges the write through the same file gate an agent's write meets, as
// an internal write (Manvi's own, so the restricted-path rung does not apply)
// with the hard rules on, then performs it through the pinned writer. The host
// posture's demotion of a taskless soft denial applies, as on policy.check.file;
// a hard denial is a refusal.
func (w harnessWriter) write(rel string, op dc.Operation, data []byte) error {
	if err := w.judge(rel, op); err != nil {
		return err
	}
	if changed, err := pinnedWrite(w.root, rel, data); err != nil {
		return &harnessMaterialiseError{cause: fmt.Errorf("writing %s: %w", rel, err), mayHaveChanged: changed}
	}
	return nil
}

// pinnedWrite is the pinned writer, and says whether a failed write may still
// have changed the file (devcouncil.WriteMayHaveChanged: the rename is done and
// the directory sync after it failed). A variable so a test can fail a write
// after its bytes are in place.
var pinnedWrite = func(root, rel string, data []byte) (mayHaveChanged bool, err error) {
	err = devcouncil.WritePinned(root, rel, data, 0o644)
	return err != nil && devcouncil.WriteMayHaveChanged(err), err
}

func (w harnessWriter) remove(rel string) error {
	if err := w.judge(rel, dc.OpDelete); err != nil {
		return err
	}
	if err := devcouncil.RemovePinned(w.root, rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return materialiseFailure("removing %s: %v", rel, err)
	}
	return nil
}

// removeRecord removes the pending record only if it is the record of the block
// with this nonce. There is one record file per checkout directory, so a run's end
// must not take away another run's: a checkout directory that was deleted and
// recreated is a new directory to the guard, which admits a second run there while
// the first is still live, and the second's record replaces the first's. The
// record is read, compared and then removed; a record that is not readable, or
// carries another nonce, is left alone. A record that vanishes between the read
// and the removal is gone, which is what was wanted.
func (w harnessWriter) removeRecord(nonce string) error {
	raw, err := devcouncil.ReadPinned(w.root, harnessPendingFile, maxPendingBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return materialiseFailure("reading the record of the harness block to remove it: %v", err)
	}
	var own struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(raw, &own) != nil || own.Nonce != nonce {
		return nil
	}
	return w.remove(harnessPendingFile)
}

func (w harnessWriter) judge(rel string, op dc.Operation) error {
	gate := policy.FileGate{Root: w.root, HardRules: true}
	d := PostureHost.demote(gate.EvaluateFileChange(rel, nil, op, true), "managed harness: Manvi's own write, no task model")
	if d.Action == policy.Deny {
		return materialiseFailure("the write gate refused %s (%s): %s", rel, d.Rule, d.Reason)
	}
	return nil
}

// harnessMaterialisation is one written block and what takes it out.
type harnessMaterialisation struct {
	harnessWriter
	// File is the instruction file the block was written to.
	File    string
	pending pendingRestore
}

// materialiseHarness writes spec's block into the provider's instruction file
// under root and returns what restores it. It returns nil, nil when there is
// nothing to write: a spec with no sections has nothing to say to the CLI. A
// provider with no instruction file is refused. Nothing already in the file is
// read as a block: content that looks like one is content. A failure leaves the
// file as it found it, with one exception: when a write that may have changed
// the file fails and the file cannot be read to tell (afterUnknownWrite), it
// returns what restores the block together with the error, and the record stays.
func materialiseHarness(root, provider string, spec harness.Spec) (*harnessMaterialisation, error) {
	rel, ok := harnessInstructionFile(provider)
	if !ok {
		return nil, materialiseFailure("provider %q has no instruction file Manvi knows how to write a harness into", provider)
	}
	nonce, err := newHarnessNonce()
	if err != nil {
		return nil, materialiseFailure("issuing a block nonce: %v", err)
	}
	block, err := renderHarnessBlock(spec, nonce)
	if err != nil || block == "" {
		return nil, err
	}
	w := harnessWriter{root: root}
	before, err := readInstructionFile(root, rel)
	existed := err == nil
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		before = nil
	default:
		return nil, materialiseFailure("reading %s: %v", rel, err)
	}
	appended := append(separator(before), block...)
	m := &harnessMaterialisation{harnessWriter: w, File: rel, pending: pendingRestore{
		Version: 1, Nonce: nonce, File: rel, Existed: existed, Offset: len(before), Appended: appended,
	}}
	if err := m.pending.valid(); err != nil {
		return nil, materialiseFailure("the block Manvi would write is not one it can later recognise: %v", err)
	}
	if err := m.pending.fitsRestoreBound(); err != nil {
		return nil, materialiseFailure("the harness block is not written: %v", err)
	}
	record, err := json.Marshal(m.pending)
	if err != nil {
		return nil, materialiseFailure("recording the block: %v", err)
	}
	op := dc.OpModify
	if !existed {
		op = dc.OpCreate
	}
	// The record first: a crash between the two writes leaves a record of a
	// block that is not there, which restores as nothing, and never a block with
	// no record.
	if err := w.write(harnessPendingFile, dc.OpWrite, record); err != nil {
		if writeMayHaveChanged(err) {
			err = errors.Join(err, w.removeRecord(nonce)) // a record of no block restores as nothing
		}
		return nil, err
	}
	whole := append(append([]byte(nil), before...), appended...)
	if err := w.write(rel, op, whole); err != nil {
		if !writeMayHaveChanged(err) {
			return nil, errors.Join(err, w.removeRecord(nonce))
		}
		return m.afterUnknownWrite(err)
	}
	return m, nil
}

// afterUnknownWrite decides what a block write that may have changed the file
// came to, by reading the file. The record is never removed while the block may
// be in it:
//
//   - the block is there: the write landed, the failure was after it, and the
//     run proceeds with what takes the block out at its end;
//   - the file is readable and the block is not there: nothing was written, a
//     failed materialisation with no record;
//   - the file cannot be read to tell: unknown. The record stays and the run is
//     returned beside the error, so the caller can still take the block out if it
//     is there, and the next Prepare on the checkout finds the record if it
//     cannot.
func (m *harnessMaterialisation) afterUnknownWrite(writeErr error) (*harnessMaterialisation, error) {
	current, err := readInstructionFile(m.root, m.File)
	switch {
	case err == nil && bytes.Contains(current, m.pending.Appended):
		return m, nil
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return nil, errors.Join(writeErr, m.removeRecord(m.pending.Nonce))
	}
	return m, errors.Join(writeErr, materialiseFailure("reading %s to see whether the block was written: %v; its record is kept", m.File, err))
}

// Restore takes the block out and clears the record. See restoreRecorded.
func (m *harnessMaterialisation) Restore() error {
	if m == nil {
		return nil
	}
	return restoreRecorded(m.harnessWriter, m.pending)
}

// restoreRecorded removes, from the file the record names, every span
// byte-identical to the recorded bytes, and nothing else; the bytes around it
// are left exactly as they are, so an agent's edits survive and a file with no
// trailing newline stays without one. Cases:
//
//   - the recorded bytes are in the file (more than once if the agent copied the
//     block): they are removed, and a file Manvi created is deleted if nothing
//     else is left in it;
//   - they are not, and no line carries the nonce: the agent removed the block
//     itself and there is nothing to undo;
//   - they are not, and a line carries the nonce: the agent edited inside the
//     block (errBlockModified). It is refused, typed, the file is left as it is
//     and the record is kept, so the next Prepare on the checkout reports it,
//     once.
//
// Every other failure (the file cannot be read, or is past the bound, or the
// write or the gate refuses) returns its error with the record kept: the block
// may still be in the file, and the record is the only thing that can take it
// out. The caller does not clear it.
func restoreRecorded(w harnessWriter, p pendingRestore) error {
	if err := p.valid(); err != nil {
		return materialiseFailure("%w: %v", errRecordUnusable, err)
	}
	current, err := readInstructionFile(w.root, p.File)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return w.removeRecord(p.Nonce) // the agent deleted the file, block and all
	default:
		return materialiseFailure("reading %s to restore it: %v", p.File, err)
	}
	remainder, removed := cutSpans(current, p.Appended, p.Offset)
	if removed == 0 {
		if bytes.Contains(current, []byte("MANVI HARNESS nonce="+p.Nonce+" ")) {
			return materialiseFailure("%w in %s: the agent edited inside it, so Manvi did "+
				"not guess where it ends and left the file as it is; remove the block from %s by hand", errBlockModified, p.File, p.File)
		}
		return w.removeRecord(p.Nonce) // the agent removed the block itself
	}
	if !p.Existed && len(bytes.TrimSpace(remainder)) == 0 {
		if err := w.remove(p.File); err != nil {
			return err
		}
	} else if err := w.write(p.File, dc.OpModify, remainder); err != nil {
		return err
	}
	return w.removeRecord(p.Nonce)
}

// cutSpans removes every non-overlapping occurrence of span from content and
// says how many it removed. hint is where the span was put; it is tried first,
// which is only a shortcut, since an agent's edit before the block moves it.
func cutSpans(content, span []byte, hint int) ([]byte, int) {
	var out []byte
	removed := 0
	rest := content
	if hint >= 0 && hint+len(span) <= len(content) && bytes.Equal(content[hint:hint+len(span)], span) {
		out = append(out, content[:hint]...)
		rest = content[hint+len(span):]
		removed++
	}
	for {
		i := bytes.Index(rest, span)
		if i < 0 {
			break
		}
		out = append(out, rest[:i]...)
		rest = rest[i+len(span):]
		removed++
	}
	if removed == 0 {
		return content, 0
	}
	return append(out, rest...), removed
}

// harnessRecordPending says whether a record of a block is on disk under root:
// a block a run wrote may still be in the instruction file.
func harnessRecordPending(root string) bool {
	_, err := devcouncil.ReadPinned(root, harnessPendingFile, maxPendingBytes)
	return err == nil
}

// recoverPendingHarness applies a record a run left behind, for a checkout no
// live run holds. It returns a note saying what it did (empty when there was
// nothing), or the typed error when the block could not be taken out.
//
// The record is cleared, so the refusal is made once and the next attempt is not
// wedged by it, only when there is nothing it could ever take out: it cannot be
// read or is not a block Manvi would have written, or the agent edited inside
// the block. Any other failure (the file cannot be read, is past the bound, the
// write is refused) keeps the record, because the block may still be in the file
// and every attempt on the checkout, and every verification of it, is refused
// until the cause is put right and a later recovery takes the block out.
func recoverPendingHarness(root string) (string, error) {
	w := harnessWriter{root: root}
	raw, err := devcouncil.ReadPinned(root, harnessPendingFile, maxPendingBytes)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	default:
		return "", materialiseFailure("reading the record of a harness block a run left behind: %v", err)
	}
	var p pendingRestore
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return "", errors.Join(materialiseFailure("the record of a harness block a run left behind is unreadable (%v); it was removed", err),
			w.remove(harnessPendingFile))
	}
	if err := restoreRecorded(w, p); err != nil {
		switch {
		case errors.Is(err, errRecordUnusable):
			err = errors.Join(err, w.remove(harnessPendingFile)) // no nonce in it to trust
		case errors.Is(err, errBlockModified):
			err = errors.Join(err, w.removeRecord(p.Nonce))
		}
		return "", err
	}
	return fmt.Sprintf("took the harness block of a run that never finished out of %s", p.File), nil
}
