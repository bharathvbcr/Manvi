package devcouncil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The streaming half of the commit-time content check.
//
// A blob is never held whole. `git cat-file --batch` writes each object as
// "<id> <type> <size>\n<bytes>\n"; batchReader unframes that stream as it
// arrives and hands the bytes to a per-object sink, so the memory a scan takes
// is one window however large the object is, and every byte read is counted
// against a budget.

const (
	// defaultScanChunkBytes is how many fresh bytes a window gathers before it
	// is searched. KnownSecret re-arms the credential scrubber on every call, so
	// it is asked once per window, never once per line of a binary.
	defaultScanChunkBytes = 1 << 20
	// defaultScanStreamBytes bounds the bytes one commit check streams from the
	// object store, across every blob.
	defaultScanStreamBytes = 1 << 30
	// defaultSecretLineBytes bounds one line of a secret-path file. A line is
	// held whole to be matched, so it is the unit that must be bounded.
	defaultSecretLineBytes = 1 << 20
	// defaultSecretTotalBytes bounds what the check reads across every secret
	// file for their values.
	defaultSecretTotalBytes = 64 << 20
	// defaultSecretValues bounds how many distinct values the check holds.
	defaultSecretValues = 1 << 20
	// defaultKnownSecretMaxLen is the overlap assumed for KnownSecret when the
	// host gave no KnownSecretMaxLen: longer than any provider key.
	defaultKnownSecretMaxLen = 4096
	// maxBatchHeaderBytes bounds a cat-file header line, which is an object id,
	// a type and a decimal size.
	maxBatchHeaderBytes = 256
)

// scanLimits are the bounds of one commit content check. A zero field is its
// default, which is how production runs; a test sets one to reach the case it
// names without a gigabyte of fixture.
type scanLimits struct {
	chunkBytes       int
	streamBytes      int64
	scanTimeout      time.Duration
	secretLineBytes  int
	secretTotalBytes int64
	secretValues     int
}

func (l scanLimits) resolved() scanLimits {
	if l.chunkBytes <= 0 {
		l.chunkBytes = defaultScanChunkBytes
	}
	if l.streamBytes <= 0 {
		l.streamBytes = defaultScanStreamBytes
	}
	if l.secretLineBytes <= 0 {
		l.secretLineBytes = defaultSecretLineBytes
	}
	if l.secretTotalBytes <= 0 {
		l.secretTotalBytes = defaultSecretTotalBytes
	}
	if l.secretValues <= 0 {
		l.secretValues = defaultSecretValues
	}
	return l
}

// commitScanSeams are the injectable parts of the commit check: its bounds, and
// how a working-tree secret file is opened. Zero is the real thing.
type commitScanSeams struct {
	limits scanLimits
	// openFile opens a working-tree secret file for its values. A test fails it
	// to stand for a read that failed after git hashed the same file, which no
	// file permission can arrange for one user.
	openFile func(path string) (io.ReadCloser, error)
}

func (s commitScanSeams) open(path string) (io.ReadCloser, error) {
	if s.openFile != nil {
		return s.openFile(path)
	}
	return os.Open(filepath.Clean(path))
}

// blobSink receives one object's bytes. Close is called when the object ended.
type blobSink interface {
	io.Writer
	Close() error
}

// batchItem is one object asked of cat-file, and the name it is reported under.
type batchItem struct {
	id, label string
}

// batchReader unframes `git cat-file --batch` output. It is the stdout writer of
// the child, so it runs on the copy goroutine; progress is read from another,
// hence the lock. After the first error it takes nothing more: the error comes
// back to the child as a failed write, which ends it.
type batchReader struct {
	mu      sync.Mutex
	items   []batchItem
	sinkFor func(batchItem) blobSink
	budget  int64

	next    int
	hdr     []byte
	cur     blobSink
	curItem batchItem
	remain  int64
	needNL  bool
	total   int64
	err     error
}

func (b *batchReader) fail(err error) (int, error) {
	if b.err == nil {
		b.err = err
	}
	return 0, b.err
}

func (b *batchReader) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	rest := p
	for len(rest) > 0 {
		switch {
		case b.cur != nil:
			n := int64(len(rest))
			if n > b.remain {
				n = b.remain
			}
			if b.total+n > b.budget {
				return b.fail(fmt.Errorf("the staged blob %s was not fully scanned: the scan stopped after %d bytes, at its %d-byte budget; "+
					"commit fewer binary files at a time", b.curItem.label, b.total, b.budget))
			}
			if _, err := b.cur.Write(rest[:n]); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return b.fail(b.cutOff())
				}
				return b.fail(err)
			}
			b.total += n
			b.remain -= n
			rest = rest[n:]
			if b.remain == 0 {
				sink := b.cur
				b.cur = nil
				b.needNL = true
				if err := sink.Close(); err != nil {
					return b.fail(err)
				}
			}
		case b.needNL:
			if rest[0] != '\n' {
				return b.fail(fmt.Errorf("git cat-file --batch framed %s badly: no newline after its bytes", b.curItem.label))
			}
			b.needNL = false
			rest = rest[1:]
		default:
			i := bytes.IndexByte(rest, '\n')
			if i < 0 {
				b.hdr = append(b.hdr, rest...)
				if len(b.hdr) > maxBatchHeaderBytes {
					return b.fail(fmt.Errorf("git cat-file --batch wrote a header longer than %d bytes", maxBatchHeaderBytes))
				}
				rest = nil
				break
			}
			b.hdr = append(b.hdr, rest[:i]...)
			rest = rest[i+1:]
			if len(b.hdr) > maxBatchHeaderBytes {
				return b.fail(fmt.Errorf("git cat-file --batch wrote a header longer than %d bytes", maxBatchHeaderBytes))
			}
			header := string(b.hdr)
			b.hdr = b.hdr[:0]
			if err := b.begin(header); err != nil {
				return b.fail(err)
			}
		}
	}
	return len(p), nil
}

// begin starts the next object from its header line.
func (b *batchReader) begin(header string) error {
	if b.next >= len(b.items) {
		return fmt.Errorf("git cat-file --batch answered more objects than were asked for: %q", header)
	}
	item := b.items[b.next]
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return fmt.Errorf("the staged blob %s (%s) is not in the object store, so it was not scanned", item.label, item.id)
	}
	if len(fields) != 3 || fields[0] != item.id || fields[1] != "blob" {
		return fmt.Errorf("git cat-file --batch answered %q for %s (%s), not a blob header", header, item.label, item.id)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("git cat-file --batch gave %s an unreadable size %q", item.label, fields[2])
	}
	b.next++
	b.curItem = item
	b.remain = size
	b.cur = b.sinkFor(item)
	if size == 0 {
		sink := b.cur
		b.cur = nil
		b.needNL = true
		return sink.Close()
	}
	return nil
}

// cutOff is the refusal for a scan the deadline ended, naming where it was.
func (b *batchReader) cutOff() error { return cutOffError(b.curItem.label, b.total) }

func cutOffError(label string, total int64) error {
	return fmt.Errorf("the scan of the staged blob %s was cut off by the deadline after %d bytes, so the rest was not scanned; "+
		"commit fewer or smaller files at a time", label, total)
}

// failure is the first error a sink or the framing raised.
func (b *batchReader) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// progress names the object being read and how many bytes have been streamed.
func (b *batchReader) progress() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	label := b.curItem.label
	if label == "" && len(b.items) > 0 {
		label = b.items[0].label
	}
	return label, b.total
}

// complete reports whether every object asked for was read to its end.
func (b *batchReader) complete() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if b.next != len(b.items) || b.cur != nil || b.needNL || len(b.hdr) > 0 {
		return fmt.Errorf("git cat-file --batch ended after %d of %d objects, so the rest were not scanned", b.next, len(b.items))
	}
	return nil
}

// streamBlobs reads the given objects through sinkFor's sinks, bounded by the
// stream budget and the context's deadline. A
// failure names what was being read when it came.
func (r *Registry) streamBlobs(ctx context.Context, items []batchItem, sinkFor func(batchItem) blobSink) error {
	if len(items) == 0 {
		return nil
	}
	limits := r.scan.limits.resolved()
	var ids strings.Builder
	for _, item := range items {
		ids.WriteString(item.id)
		ids.WriteByte('\n')
	}
	reader := &batchReader{items: items, sinkFor: sinkFor, budget: limits.streamBytes}
	spec := gitSpec(r.deps.Root, "", []string{"cat-file", "--batch"})
	spec.stdin = strings.NewReader(ids.String())
	spec.stdout = reader
	res := runProc(ctx, spec)
	// The reader's own error first: it ended the child through a failed write,
	// so the child's exit status says only that it was cut off.
	if err := reader.failure(); err != nil {
		return err
	}
	if res.timedOut {
		label, total := reader.progress()
		return cutOffError(label, total)
	}
	if res.err != nil {
		stderr := strings.TrimSpace(string(res.stderr))
		if len(stderr) > gitStderrPreview {
			stderr = stderr[:gitStderrPreview] + "…"
		}
		return fmt.Errorf("git cat-file --batch: %w: %s", res.err, stderr)
	}
	return reader.complete()
}

// windowNeedle is one value to find, and why finding it is a leak.
type windowNeedle struct {
	reason string
	bytes  []byte
}

// windowSink searches an object's bytes for the values the commit check holds.
// It keeps the last overlap bytes of each window in front of the next, so a
// value that straddles two windows is still found whole: overlap is one short
// of the longest value, which is exactly enough for a value to lie wholly in
// one window.
type windowSink struct {
	ctx     context.Context
	label   string
	chunk   int
	overlap int
	values  []windowNeedle
	known   func(string) bool
	hit     func(path, reason string)

	buf   []byte
	fresh int
}

func (s *windowSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	s.fresh += len(p)
	if s.fresh >= s.chunk {
		if err := s.scan(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (s *windowSink) Close() error {
	if s.fresh > 0 {
		return s.scan()
	}
	return nil
}

func (s *windowSink) scan() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for _, n := range s.values {
		if bytes.Contains(s.buf, n.bytes) {
			s.hit(s.label, n.reason)
		}
	}
	if s.known != nil && s.known(string(s.buf)) {
		s.hit(s.label, "contains a credential this process holds")
	}
	keep := len(s.buf)
	if keep > s.overlap {
		keep = s.overlap
	}
	n := copy(s.buf, s.buf[len(s.buf)-keep:])
	s.buf = s.buf[:n]
	s.fresh = 0
	return nil
}

// scanNote records a check that a bound held back without refusing the commit:
// which bound, on which file, what was still checked, and who can cure it. It is
// for bounds the agent cannot cure — a secret-path file is write-protected, so
// the agent can neither shrink nor move it — where a refusal would be a lockout
// rather than a remedy. It never carries a value.
type scanNote struct {
	Kind           string `json:"kind"`
	File           string `json:"file"`
	Bound          string `json:"bound"`
	StillChecked   string `json:"still_checked"`
	OperatorAction string `json:"operator_action"`
}

// degraded is the note's entry in tools.Result.Degraded, the list the session
// log records for a result that is not a clean pass.
func (n scanNote) degraded() string { return "commit.scan." + n.Kind + ": " + n.File }

// degradedList is every note's degraded entry.
func degradedList(notes []scanNote) []string {
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.degraded()
	}
	return out
}

const operatorOnly = "an operator can split the file or move it out of the repository; the agent cannot, because secret paths are write-protected for it"

// valueScan collects the credential-looking assignments of secret-path files
// into a secretFileSet, within bounds. A bound it reaches is a scanNote, never
// a refusal and never a silent drop: what was collected before the bound is
// still matched, and the note says what the bound cost.
type valueScan struct {
	set    *secretFileSet
	limits scanLimits
	total  int64
	// exhausted: the total byte bound is spent; full: the value bound is.
	exhausted, full bool
	noted           map[string]bool
	notes           []scanNote
}

// noteOnce records one note per kind and file.
func (v *valueScan) noteOnce(n scanNote) {
	key := n.Kind + "\x00" + n.File
	if v.noted == nil {
		v.noted = map[string]bool{}
	}
	if v.noted[key] {
		return
	}
	v.noted[key] = true
	v.notes = append(v.notes, n)
}

func (v *valueScan) noteBound(kind, file, bound string) {
	v.noteOnce(scanNote{
		Kind: kind, File: file, Bound: bound,
		StillChecked: fmt.Sprintf("staged blobs are still compared by blob id with every version git holds of this file (exact copies), "+
			"and searched for the %d value(s) collected before the bound", len(v.set.values)),
		OperatorAction: operatorOnly,
	})
}

// noteUnreadable records a working-tree copy that could not be opened or read.
// A tracked file is still judged by the versions git holds; an untracked one has
// nothing else to be judged by.
func (v *valueScan) noteUnreadable(path string, tracked bool, err error) {
	still := "nothing: the file is untracked, so neither its blob id nor its values were collected"
	if tracked {
		still = "the index and HEAD versions of the file, by blob id and by value; its working-tree copy was neither hashed nor read, " +
			"so a staged copy of an unstaged edit is not matched by blob id"
	}
	v.noteOnce(scanNote{
		Kind: "unreadable_worktree", File: path,
		Bound:          fmt.Sprintf("the working-tree copy could not be opened or read (%v)", err),
		StillChecked:   still,
		OperatorAction: "make the file readable by the harness user, or restore it; the agent cannot change a secret path's permissions",
	})
}

// sink returns the sink that reads one secret file's lines under the name path.
func (v *valueScan) sink(path string) blobSink {
	return &valueSink{scan: v, path: path}
}

type valueSink struct {
	scan      *valueScan
	path      string
	line      []byte
	lineNo    int
	skipping  bool // inside a line over the cap, dropped to its newline
	truncated bool // the total bound cut this file mid-stream
}

func (s *valueSink) Write(p []byte) (int, error) {
	written := len(p)
	sc := s.scan
	if sc.exhausted {
		s.truncated = true
		sc.noteBound("total_cap", s.path, fmt.Sprintf("the secret-path files hold more than the %d bytes this check reads for their values", sc.limits.secretTotalBytes))
		return written, nil
	}
	if room := sc.limits.secretTotalBytes - sc.total; int64(len(p)) > room {
		p = p[:room]
		sc.exhausted, s.truncated = true, true
		sc.noteBound("total_cap", s.path, fmt.Sprintf("the secret-path files hold more than the %d bytes this check reads for their values (reading stopped in %s)", sc.limits.secretTotalBytes, s.path))
	}
	sc.total += int64(len(p))
	for len(p) > 0 {
		chunk, rest := p, []byte(nil)
		i := bytes.IndexByte(p, '\n')
		if i >= 0 {
			chunk, rest = p[:i], p[i+1:]
		}
		if !s.skipping {
			s.line = append(s.line, chunk...)
			if len(s.line) > sc.limits.secretLineBytes {
				s.line, s.skipping = s.line[:0], true
				sc.noteBound("line_cap", s.path, fmt.Sprintf("line %d is over the %d-byte per-line cap this check reads for values, so that line was not checked", s.lineNo+1, sc.limits.secretLineBytes))
			}
		}
		if i < 0 {
			break
		}
		if s.skipping {
			s.skipping = false
			s.lineNo++
		} else {
			s.flush()
		}
		p = rest
	}
	return written, nil
}

func (s *valueSink) Close() error {
	// A line the total bound cut in half is not a line: its value would be a
	// prefix of the real one.
	if len(s.line) > 0 && !s.truncated {
		s.flush()
	}
	return nil
}

// flush reads the assignment on the buffered line, if it is one.
func (s *valueSink) flush() {
	line := s.line
	s.line = s.line[:0]
	s.lineNo++
	m := assignedValue.FindStringSubmatch(string(line))
	if m == nil {
		return
	}
	value := strings.Trim(m[2], `"'`)
	if !looksCredential(m[1], value) {
		return
	}
	sc := s.scan
	if _, known := sc.set.values[value]; !known && len(sc.set.values) >= sc.limits.secretValues {
		sc.full = true
		sc.noteBound("value_cap", s.path, fmt.Sprintf("the secret-path files assign more than the %d credential values this check holds (reading stopped adding at line %d of %s)", sc.limits.secretValues, s.lineNo, s.path))
		return
	}
	sc.set.values[value] = s.path
}

// readValues reads one working-tree secret file's values through the open seam.
// A file that cannot be opened or read is a note, not a refusal: see scanNote.
func (r *Registry) readValues(v *valueScan, path, abs string, tracked bool) {
	f, err := r.scan.open(abs)
	if err != nil {
		v.noteUnreadable(path, tracked, err)
		return
	}
	// Read-only: a close that fails has lost nothing that was read.
	defer func() { _ = f.Close() }()
	sink := v.sink(path)
	buf := make([]byte, 64<<10)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := sink.Write(buf[:n]); err != nil {
				v.noteUnreadable(path, tracked, err)
				return
			}
		}
		if readErr == io.EOF {
			if err := sink.Close(); err != nil {
				v.noteUnreadable(path, tracked, err)
			}
			return
		}
		if readErr != nil {
			v.noteUnreadable(path, tracked, readErr)
			return
		}
	}
}
