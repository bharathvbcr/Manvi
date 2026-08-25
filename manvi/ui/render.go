package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"manvi/credentials"
	"manvi/flags"
)

// Palette holds the escape sequences the renderer uses.
//
// Colour is the renderer's own output, never content's, which is why the
// sanitizer can strip every escape from text without breaking the display: the
// styling is applied outside the sanitized region.
type Palette struct {
	Reset, Dim, Bold                        string
	Red, Yellow, Green, Blue, Cyan, Magenta string
}

// ColorPalette is the ANSI palette.
func ColorPalette() Palette {
	return Palette{
		Reset: "\x1b[0m", Dim: "\x1b[2m", Bold: "\x1b[1m",
		Red: "\x1b[31m", Yellow: "\x1b[33m", Green: "\x1b[32m",
		Blue: "\x1b[34m", Cyan: "\x1b[36m", Magenta: "\x1b[35m",
	}
}

// PlainPalette renders without colour, for a pipe, a CI log, or NO_COLOR.
func PlainPalette() Palette { return Palette{} }

// Decorator is implemented by a writer that wraps another one.
//
// Whether output should be coloured is a question about the far end of the
// chain, not about what sits in front of it. Without this seam a writer that
// merely watches bytes on their way past — counting them, or remembering a
// write that failed — silently turns colour off for an operator at a real
// terminal, because the *os.File it decorates is no longer the value being
// asked. The decoration is invisible to the reader and so is the regression.
type Decorator interface {
	// Underlying returns the writer this one writes through.
	Underlying() io.Writer
}

// TerminalFile returns the file a writer ultimately writes to.
//
// The walk is bounded because a decorator that returns itself, directly or
// through a cycle, must cost a wrong answer rather than a hung renderer.
func TerminalFile(w io.Writer) (*os.File, bool) {
	for range 8 {
		switch v := w.(type) {
		case *os.File:
			return v, true
		case Decorator:
			w = v.Underlying()
		default:
			return nil, false
		}
	}
	return nil, false
}

// ShouldColor reports whether output to w should be coloured.
//
// Three checks, in the order that respects the operator: an explicit NO_COLOR
// wins, then whether the destination is actually a terminal. Writing escape
// codes into a redirected file is how logs become unreadable.
func ShouldColor(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := TerminalFile(w)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Renderer writes events to a terminal.
type Renderer struct {
	mu      sync.Mutex
	out     io.Writer
	palette Palette
	// scrubber removes credential values from everything on its way out. It is
	// the backstop behind the Secret type, for the case where a provider echoes
	// a key back inside an error body.
	scrubber *credentials.Scrubber
	// MaxToolResult bounds how much of a tool result is shown. A tool that
	// returns a whole file would otherwise scroll the transcript away.
	MaxToolResult int
	// streaming tracks whether the last thing written was assistant text, so
	// deltas concatenate on one line and a following event starts a new one.
	streaming bool
	// writeErr holds the first write that did not land. A terminal that is a
	// terminal never fails, but this face's output is also what a caller gets
	// when they redirect it, and a run whose account of itself was cut short
	// must not report the status of one that was written whole.
	writeErr error
}

// NewRenderer builds a terminal renderer.
func NewRenderer(out io.Writer, scrubber *credentials.Scrubber) *Renderer {
	palette := PlainPalette()
	if ShouldColor(out) {
		palette = ColorPalette()
	}
	if scrubber == nil {
		scrubber = credentials.NewScrubber()
	}
	return &Renderer{out: out, palette: palette, scrubber: scrubber, MaxToolResult: 2000}
}

// SetPalette overrides colour selection, which is what makes rendering testable
// without a terminal.
func (r *Renderer) SetPalette(p Palette) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.palette = p
}

// safe prepares untrusted text for a terminal: credentials removed, control
// sequences neutralised. Every path that writes content goes through it.
func (r *Renderer) safe(text string) string {
	return Sanitize(r.scrubber.Clean(text))
}

func (r *Renderer) write(format string, args ...any) {
	if _, err := fmt.Fprintf(r.out, format, args...); err != nil && r.writeErr == nil {
		r.writeErr = err
	}
}

// Err reports the first write this renderer could not make, or nil. It is the
// same question JSONSink.Err answers, asked of the other face, so a caller can
// ask it of whichever one a run was given without knowing which that was.
func (r *Renderer) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeErr
}

// endStream closes an in-progress assistant line before another kind of event
// is written, so streamed text is never run together with a tool banner.
func (r *Renderer) endStream() {
	if r.streaming {
		r.write("\n")
		r.streaming = false
	}
}

// Emit renders one event.
func (r *Renderer) Emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.palette

	// Attribution first, on its own, so the line that follows is read as the
	// child's. Event.Agent's doc states the condition this satisfies: two
	// agents' evidence in one stream is only readable if each line says whose
	// it is. Without it a fan-out's tool banners and gate decisions interleave
	// into the parent's transcript looking like the parent's own.
	if e.Agent != "" {
		r.endStream()
		r.write("%s[%s]%s ", p.Dim, r.safe(e.Agent), p.Reset)
	}

	switch e.Kind {
	case KindSessionStart:
		r.endStream()
		r.write("%s%s manvi%s  posture %s%s%s  model %s\n",
			p.Bold, p.Cyan, p.Reset, p.Bold, r.safe(e.Posture), p.Reset, r.safe(e.Model))
		if effect := flags.DescribePosture(e.Posture); effect.Relaxed {
			// Stated every session, not once. The posture changes what a clean
			// run means, and an operator who does not see it will read a green
			// result as an enforced one.
			r.write("%s  %s%s\n", p.Dim, effect.Notice, p.Reset)
		}

	case KindTurnStart:
		r.endStream()
		r.write("\n%s▌%s %s\n", p.Blue, p.Reset, r.safe(e.Text))

	case KindReasoning:
		r.endStream()
		r.write("%s%s%s\n", p.Dim, r.safe(Truncate(e.Text, 400)), p.Reset)

	case KindText:
		// Streamed: written without a newline so deltas concatenate.
		r.write("%s", r.safe(e.Text))
		r.streaming = true

	case KindToolStart:
		r.endStream()
		r.write("%s  ⚙ %s%s %s\n", p.Cyan, r.safe(e.Tool), p.Reset, p.Dim+r.safe(e.Detail)+p.Reset)

	case KindToolResult:
		r.endStream()
		marker, colour := "✓", p.Green
		if e.IsError {
			marker, colour = "✗", p.Red
		}
		body := r.safe(Truncate(e.Text, r.MaxToolResult))
		r.write("%s  %s%s %s\n", colour, marker, p.Reset, indent(body, "    "))
		r.writeQualification(e)

	case KindPolicy:
		r.endStream()
		r.writePolicy(e)

	case KindApproval:
		r.endStream()
		r.write("\n%s%s ⚠ %s%s\n", p.Bold, p.Yellow, r.safe(e.Text), p.Reset)
		// A question raised through this same seam has no blocked path. Printing
		// the rule row anyway would report a rule that fired on nothing.
		if e.Path != "" {
			r.write("    rule %s%s%s  severity %s  path %s\n",
				p.Bold, r.safe(e.Rule), p.Reset, r.safe(e.Severity), r.safe(e.Path))
		}

	case KindApprovalDone:
		r.endStream()
		r.write("    %s→ %s%s\n", p.Dim, r.safe(e.Text), p.Reset)

	case KindLease:
		r.endStream()
		r.write("%s  ⌁ %s%s\n", p.Magenta, r.safe(e.Text), p.Reset)

	case KindUsage:
		r.endStream()
		r.write("%s  %d in / %d out tokens%s\n", p.Dim, e.InputTokens, e.OutputTokens, p.Reset)

	case KindTurnEnd:
		r.endStream()

	case KindReport:
		r.endStream()
		r.write("\n%s%s%s\n", p.Bold, r.safe(e.Text), p.Reset)

	case KindError:
		r.endStream()
		r.write("%s  ✗ %s%s\n", p.Red, r.safe(e.Text), p.Reset)

	case KindNotice:
		r.endStream()
		r.write("%s  %s%s\n", p.Dim, r.safe(e.Text), p.Reset)
	}
}

// writePolicy renders a decision, including the ones that allowed. A blocked
// write and a write allowed only because the posture is dev are different
// facts, and both need to be visible.
func (r *Renderer) writePolicy(e Event) {
	p := r.palette
	switch {
	case e.Severity == "hard":
		r.write("%s  ⛔ %s%s %s%s%s\n", p.Red, r.safe(e.Text), p.Reset,
			p.Dim, r.safe(e.Rule), p.Reset)
		r.write("      %sthis rule is never grantable; change the approach%s\n", p.Dim, p.Reset)
	case e.Demoted != "":
		r.write("%s  ⚠ %s%s\n", p.Yellow, r.safe(e.Text), p.Reset)
		r.write("      %swould have blocked: %s — allowed by %s%s\n",
			p.Dim, r.safe(e.Rule), r.safe(e.Demoted), p.Reset)
	case e.GrantID != "":
		r.write("%s  ✓ %s%s %s(%s cleared %s, %s)%s\n", p.Yellow, r.safe(e.Text), p.Reset,
			p.Dim, r.safe(e.GrantID), r.safe(e.Rule), r.safe(e.GrantedBy), p.Reset)
	default:
		r.write("%s  ⚠ %s%s %s%s%s\n", p.Yellow, r.safe(e.Text), p.Reset,
			p.Dim, r.safe(e.Rule), p.Reset)
	}
	r.writeDegraded(e)
	r.writeWeakened(e)
}

func (r *Renderer) writeQualification(e Event) {
	if !e.Qualified() {
		return
	}
	p := r.palette
	if e.Demoted != "" {
		r.write("      %s%s would have blocked this — allowed by %s%s\n",
			p.Dim, r.safe(e.Rule), r.safe(e.Demoted), p.Reset)
	}
	if e.GrantID != "" {
		r.write("      %scleared by %s (%s)%s\n", p.Dim, r.safe(e.GrantID), r.safe(e.GrantedBy), p.Reset)
	}
	r.writeDegraded(e)
	r.writeWeakened(e)
}

// writeDegraded names checks that did not run. Printing nothing here is what
// would make an unexamined result look examined.
func (r *Renderer) writeDegraded(e Event) {
	if len(e.Degraded) == 0 {
		return
	}
	p := r.palette
	safe := make([]string, len(e.Degraded))
	for i, d := range e.Degraded {
		safe[i] = r.safe(d)
	}
	r.write("      %schecks that did not run: %s%s\n", p.Dim, strings.Join(safe, ", "), p.Reset)
}

// writeWeakened names safety settings that were relaxed for this run.
func (r *Renderer) writeWeakened(e Event) {
	if len(e.Weakened) == 0 {
		return
	}
	p := r.palette
	safe := make([]string, len(e.Weakened))
	for i, w := range e.Weakened {
		safe[i] = r.safe(w)
	}
	r.write("      %ssafety settings off default: %s%s\n", p.Yellow, strings.Join(safe, ", "), p.Reset)
}

func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// JSONSink writes newline-delimited JSON, the headless face.
//
// It consumes the same events as the terminal renderer rather than a parallel
// set, which is what keeps the two faces from drifting: an event a CI job
// cannot see is one the terminal should not be showing either.
type JSONSink struct {
	mu  sync.Mutex
	out io.Writer
	// writeErr holds the first line that could not be written out. Emit
	// satisfies Sink, which returns nothing, so the failure is kept here for
	// the caller to ask about rather than dropped: a transcript cut short by a
	// full disk or a closed pipe reads exactly like a run that ended there.
	writeErr error
	scrubber *credentials.Scrubber
}

// NewJSONSink builds a JSON writer.
func NewJSONSink(out io.Writer, scrubber *credentials.Scrubber) *JSONSink {
	if scrubber == nil {
		scrubber = credentials.NewScrubber()
	}
	return &JSONSink{out: out, scrubber: scrubber}
}

// Emit writes one event as a JSON line.
//
// Control sequences are left intact here and credentials are not: a consumer
// of this stream is a program, and escaping its text would corrupt the record,
// whereas a credential in it is a credential on disk.
//
// Marshalling is done here rather than by a json.Encoder because the two ways
// this can fail need different answers, and an encoder reports them as one
// error after the fact. A value that will not marshal is answered with a line
// that says so; a writer that will not take the line is remembered for Err.
func (s *JSONSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Text = s.scrubber.Clean(e.Text)
	e.Detail = s.scrubber.Clean(e.Detail)

	line, err := json.Marshal(e)
	if err != nil {
		line = s.holeLine(e, err)
	}
	if _, err := s.out.Write(append(line, '\n')); err != nil && s.writeErr == nil {
		s.writeErr = err
	}
}

// Err reports the first line this sink failed to write, or nil.
//
// A caller that finishes a run with a non-nil error here holds an incomplete
// transcript and must not report the run as recorded. Nothing else can tell:
// the missing lines are missing.
func (s *JSONSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// holeLine builds the line written in place of an event that will not marshal.
//
// json.Encoder writes nothing at all when marshalling fails, so discarding its
// error deleted the line: an event whose arguments were malformed and an event
// that never happened were the same thing to a CI job reading the stream, and
// this harness's rule is that a check which could not run must never look like
// one that ran and passed. Arguments is the field that can fail — json.RawMessage
// validates on marshal and holds bytes the model wrote — so the replacement
// drops it, names it, and keeps the rest of the event. The record then has a
// hole in it rather than a hole where it should have been.
func (s *JSONSink) holeLine(e Event, cause error) []byte {
	var dropped []string
	if len(e.Arguments) > 0 && !json.Valid(e.Arguments) {
		dropped = append(dropped, "arguments")
	}
	e.Arguments = nil
	e.EncodeError = s.scrubber.Clean(encodeNote(dropped, cause))
	if line, err := json.Marshal(e); err == nil {
		return line
	}

	// After Arguments the only field left that can refuse to marshal is the
	// timestamp: time.Time rejects a year outside [0,9999], and every other
	// field is a string, a bool, or an int. Dropping it leaves a value that
	// cannot fail.
	e.At = time.Time{}
	e.EncodeError = s.scrubber.Clean(encodeNote(append(dropped, "at"), cause))
	if line, err := json.Marshal(e); err == nil {
		return line
	}

	// Unreachable while Event holds no other marshaller, and here because the
	// alternative at this point is writing nothing, which is the bug.
	return []byte(`{"kind":"error","encode_error":"this event could not be serialised"}`)
}

// encodeNote names what the line is missing and why, in the terms a reader of
// the transcript needs to act: which field is gone, and what was wrong with it.
func encodeNote(dropped []string, cause error) string {
	if len(dropped) == 0 {
		dropped = []string{"an unidentified field"}
	}
	return fmt.Sprintf("dropped from this line: %s (%v)", strings.Join(dropped, ", "), cause)
}
