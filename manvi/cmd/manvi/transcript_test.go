package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"manvi/credentials"
	"manvi/ui"
)

// brokenWriter is a transcript destination that refuses every line: a full
// disk, a closed pipe, a revoked file handle.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func brokenSink(t *testing.T) ui.Sink {
	t.Helper()
	sink := ui.NewJSONSink(brokenWriter{}, nil)
	sink.Emit(ui.Event{Kind: ui.KindReport, Text: "done"})
	if sink.Err() == nil {
		t.Fatal("the sink did not notice a writer that refused the line")
	}
	return sink
}

// TestACleanTurnWithABrokenTranscriptDoesNotExitZero is the whole point of the
// seam. The turn succeeded and the record of it is short; a caller handed exit
// 0 would read a truncated transcript as the complete account of a clean run,
// which is the same failure as a check that could not run reporting a pass.
func TestACleanTurnWithABrokenTranscriptDoesNotExitZero(t *testing.T) {
	var notes bytes.Buffer
	err := outputStatus(nil, &notes, nil, faceFailure(brokenSink(t)))
	if err == nil {
		t.Fatal("a run whose transcript could not be written reported success")
	}
	if !strings.Contains(err.Error(), "output is incomplete") {
		t.Fatalf("the error does not say what went wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("the error does not carry the underlying cause: %v", err)
	}
	// main prints a returned error itself; saying it here too would print the
	// same line twice.
	if notes.Len() != 0 {
		t.Fatalf("the returned error was also written to stderr: %q", notes.String())
	}
}

// TestTheTurnsOwnSentinelSurvivesABrokenTranscript: the sentinels map to
// specific exit statuses a caller branches on, and each says something more
// specific about the same run than "the record is short" does. It wins — but
// it does not get to bury the other fact.
func TestTheTurnsOwnSentinelSurvivesABrokenTranscript(t *testing.T) {
	var notes bytes.Buffer
	err := outputStatus(errTruncated, &notes, nil, faceFailure(brokenSink(t)))
	if !errors.Is(err, errTruncated) {
		t.Fatalf("the turn's own status was replaced: %v", err)
	}
	if !strings.Contains(notes.String(), "output is incomplete") {
		t.Fatalf("the incomplete output went unreported on stderr: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "no space left on device") {
		t.Fatalf("stderr does not carry the underlying cause: %q", notes.String())
	}
}

// TestAWholeOutputChangesNothing: the ordinary path must stay ordinary, in
// every one of the three output modes `manvi run` has.
func TestAWholeOutputChangesNothing(t *testing.T) {
	var out bytes.Buffer
	healthy := ui.NewJSONSink(&out, nil)
	healthy.Emit(ui.Event{Kind: ui.KindReport, Text: "done"})
	terminal := ui.NewRenderer(&out, nil)
	terminal.Emit(ui.Event{Kind: ui.KindReport, Text: "done"})

	for name, sink := range map[string]ui.Sink{
		"json":     healthy,
		"terminal": terminal,
		"quiet":    ui.SinkFunc(func(ui.Event) {}),
	} {
		var notes bytes.Buffer
		if err := outputStatus(nil, &notes, nil, faceFailure(sink)); err != nil {
			t.Errorf("%s: a clean run was failed: %v", name, err)
		}
		if err := outputStatus(errNoAnswer, &notes, nil, faceFailure(sink)); !errors.Is(err, errNoAnswer) {
			t.Errorf("%s: the turn's own status was changed: %v", name, err)
		}
		if notes.Len() != 0 {
			t.Errorf("%s: output that landed was reported as lost: %q", name, notes.String())
		}
	}
}

// TestTheTerminalFaceIsHeldToTheSameRule: --json is not the only mode whose
// output a caller redirects, and the check has to cover the face a run
// actually got rather than the one it was written for.
func TestTheTerminalFaceIsHeldToTheSameRule(t *testing.T) {
	terminal := ui.NewRenderer(brokenWriter{}, nil)
	terminal.Emit(ui.Event{Kind: ui.KindReport, Text: "3 files changed"})

	err := outputStatus(nil, nil, nil, faceFailure(terminal))
	if err == nil {
		t.Fatal("a run whose terminal output could not be written reported success")
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("the error does not carry the underlying cause: %v", err)
	}
}

// TestTheQuietAnswerIsPartOfTheOutput. In --quiet the sink is a no-op and that
// one line is the entire deliverable, so it does not pass through any face —
// it is written directly. A lost answer with exit 0 is a script reading an
// empty string as the model's response.
func TestTheQuietAnswerIsPartOfTheOutput(t *testing.T) {
	quiet := ui.SinkFunc(func(ui.Event) {})
	answerErr := errors.New("no space left on device")

	err := outputStatus(nil, nil, nil, faceFailure(quiet), answerErr)
	if err == nil {
		t.Fatal("a lost --quiet answer reported success")
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("the error does not carry the underlying cause: %v", err)
	}

	// Order is stated, so pin it: a face that could not write its lines is the
	// broader failure and is reported over the single direct write.
	both := outputStatus(nil, nil, nil, faceFailure(brokenSink(t)), errors.New("the direct write"))
	if strings.Contains(both.Error(), "the direct write") {
		t.Fatalf("the direct write was reported over the face's own failure: %v", both)
	}
}

// TestABrokenTranscriptDoesNotLeakACredential. The failure text comes from the
// writer, and a writer whose error names the path or the URL it was opened
// from can carry one. This report goes to stderr and to an exit-status message,
// both of which are captured by CI.
func TestABrokenTranscriptDoesNotLeakACredential(t *testing.T) {
	const key = "xai-secret-value-abcdefghij"
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(key, "XAI_API_KEY"))

	sink := ui.NewJSONSink(leakyWriter{key: key}, nil)
	sink.Emit(ui.Event{Kind: ui.KindReport, Text: "done"})

	err := outputStatus(nil, nil, scrubber.Clean, faceFailure(sink))
	if err == nil {
		t.Fatal("a run whose transcript could not be written reported success")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("a credential reached the exit-status message: %v", err)
	}
}

type leakyWriter struct{ key string }

func (w leakyWriter) Write([]byte) (int, error) {
	return 0, errors.New("upload to https://sink.example/?token=" + w.key + " failed")
}

// TestEveryCommandsAnswerIsBackstopped drives the real dispatch.
//
// Around a hundred and sixty writes produce the answers of `manvi flags`,
// `manvi tools`, `manvi providers` and the rest, and not one of them checked
// whether the write landed. A command whose answer was truncated by a full
// disk still exited 0, so a caller parsing that answer read a fragment as the
// whole of it. The backstop is at the composition root; this asserts it is
// actually wired to the writer every command is given.
func TestEveryCommandsAnswerIsBackstopped(t *testing.T) {
	for _, args := range [][]string{
		{"--version"},
		{"help"},
		{"providers"},
		{"tools"},
	} {
		answer := &answerWriter{to: brokenWriter{}}
		var notes bytes.Buffer

		status := outputStatus(run(answer, &notes, args), &notes, nil, answer.Err())
		if status == nil {
			t.Errorf("manvi %v: the answer was lost and the command reported success", args)
			continue
		}
		if !errors.Is(status, errOutputLost) {
			t.Errorf("manvi %v: the lost answer was reported as something else: %v", args, status)
		}
	}
}

// TestTheBackstopStaysQuietWhenTheAnswerLands: the ordinary path is every run,
// and a backstop that fires on a healthy one is worse than none.
func TestTheBackstopStaysQuietWhenTheAnswerLands(t *testing.T) {
	answer := &answerWriter{to: &bytes.Buffer{}}
	var notes bytes.Buffer

	if status := outputStatus(run(answer, &notes, []string{"--version"}), &notes, nil, answer.Err()); status != nil {
		t.Fatalf("a command whose answer landed was failed: %v", status)
	}
	if answer.Err() != nil {
		t.Fatalf("a healthy writer reported a failure: %v", answer.Err())
	}
	if notes.Len() != 0 {
		t.Fatalf("a healthy run wrote to stderr: %q", notes.String())
	}
}

// TestOneLostAnswerIsReportedOnce. Two rungs check this — a command that knows
// which of its own writes was lost, and the root backstopping the rest — and
// nesting them naively said the same thing twice, which reads as two failures.
func TestOneLostAnswerIsReportedOnce(t *testing.T) {
	var notes bytes.Buffer
	inner := outputStatus(nil, &notes, nil, errors.New("no space left on device"))
	if !errors.Is(inner, errOutputLost) {
		t.Fatalf("the inner rung did not report a lost answer: %v", inner)
	}
	outer := outputStatus(inner, &notes, nil, errors.New("no space left on device"))
	if !errors.Is(outer, errOutputLost) {
		t.Fatalf("the outer rung dropped the inner report: %v", outer)
	}
	if notes.Len() != 0 {
		t.Fatalf("one lost answer was reported twice: %q", notes.String())
	}
}

// TestTheDecoratedStdoutIsTransparent: it is in the path of every byte the CLI
// writes, so it must pass writes through unchanged and let colour detection
// see the screen behind it.
func TestTheDecoratedStdoutIsTransparent(t *testing.T) {
	var sink bytes.Buffer
	answer := &answerWriter{to: &sink}
	if _, err := answer.Write([]byte("hello")); err != nil {
		t.Fatalf("a healthy write failed: %v", err)
	}
	if sink.String() != "hello" {
		t.Fatalf("the wrapper altered the bytes: %q", sink.String())
	}
	if _, ok := any(answer).(ui.Decorator); !ok {
		t.Fatal("the wrapper is not a ui.Decorator, so colour detection asks it instead of the screen")
	}
	if answer.Underlying() != io.Writer(&sink) {
		t.Fatal("the wrapper does not name what it writes through")
	}
}

// TestTheProcessStatusSaysTheAnswerWasLost is the whole fix, at the level a
// caller actually sees.
//
// Driven against a real command through the real dispatch, with a stdout that
// refuses every write. Before the backstop, this exact invocation exited 0
// having written nothing — verified against a binary built from the previous
// commit — so a script parsing `manvi tools` read an empty tool list as the
// harness having no tools.
func TestTheProcessStatusSaysTheAnswerWasLost(t *testing.T) {
	var notes bytes.Buffer
	status := exitStatus(brokenWriter{}, &notes, []string{"tools"})
	if status == 0 {
		t.Fatal("a command whose answer was never written exited 0")
	}
	if !strings.Contains(notes.String(), "output is incomplete") {
		t.Fatalf("nothing on stderr said the answer was lost: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "no space left on device") {
		t.Fatalf("stderr does not carry the underlying cause: %q", notes.String())
	}
}

// TestTheProcessStatusIsUnchangedWhenTheAnswerLands, in both of the shapes
// that exit 0: an answer, and a request for help.
func TestTheProcessStatusIsUnchangedWhenTheAnswerLands(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"tools"}, {"help"}, {"providers"}} {
		var out, notes bytes.Buffer
		if status := exitStatus(&out, &notes, args); status != 0 {
			t.Errorf("manvi %v: exited %d — %q", args, status, notes.String())
		}
		if out.Len() == 0 {
			t.Errorf("manvi %v: exited 0 having written no answer at all", args)
		}
	}
}

// TestTheHelpTextIsBackstoppedToo. It is written after the command returns,
// which is exactly the kind of write a check placed one line too early misses.
func TestTheHelpTextIsBackstoppedToo(t *testing.T) {
	var notes bytes.Buffer
	if status := exitStatus(brokenWriter{}, &notes, []string{}); status == 0 {
		t.Fatalf("usage text that was never written exited 0: %q", notes.String())
	}
}

// TestExitStatusesAreTheDocumentedOnes pins the mapping a benchmark branches
// on and the CLI reference documents. It lived inline in main until now, where
// nothing could execute it.
func TestExitStatusesAreTheDocumentedOnes(t *testing.T) {
	for _, c := range []struct {
		name     string
		sentinel error
		want     int
	}{
		{"a clean run", nil, 0},
		{"truncated by the step ceiling", errTruncated, 2},
		{"truncated by the output cap", errOutputCap, 3},
		{"no answer at all", errNoAnswer, 4},
		{"an unfinished stop reason", errUnfinished, 5},
		{"an ordinary failure", errors.New("something else"), 1},
		{"a lost answer", fmt.Errorf("%w: disk full", errOutputLost), 1},
	} {
		var notes bytes.Buffer
		if got := statusFor(c.sentinel, &notes); got != c.want {
			t.Errorf("%s mapped to exit %d, want %d", c.name, got, c.want)
		}
	}
}

// TestEveryFailureExplainsItselfSomewhere: the four sentinels are documented
// as having said their piece already, and the default case is the only one
// that still has to speak. A silent non-zero status is a caller with a number
// and no reason.
func TestEveryFailureExplainsItselfSomewhere(t *testing.T) {
	var notes bytes.Buffer
	if statusFor(errors.New("the store is unreachable"), &notes); notes.Len() == 0 {
		t.Fatal("an ordinary failure exited non-zero and said nothing")
	}
	if !strings.Contains(notes.String(), "the store is unreachable") {
		t.Fatalf("the reason was replaced: %q", notes.String())
	}
}

// TestACommandThatReportsItsOwnLostWriteIsNotEchoed.
//
// Some commands are nothing but a write — `manvi logo` renders the mark and
// returns whatever writing it returned — so the failure comes back as the
// command's own error and is printed with it. The backstop saw the same write
// fail and would have added a second line for the same event, which reads as
// two failures where there was one.
func TestACommandThatReportsItsOwnLostWriteIsNotEchoed(t *testing.T) {
	lost := errors.New("write /dev/stdout: bad file descriptor")
	var notes bytes.Buffer

	status := outputStatus(lost, &notes, nil, lost)
	if !errors.Is(status, lost) {
		t.Fatalf("the command's own error was replaced: %v", status)
	}
	if notes.Len() != 0 {
		t.Fatalf("the same lost write was reported twice: %q", notes.String())
	}

	// A different failure is still a second fact and still gets said.
	notes.Reset()
	if status := outputStatus(errNoAnswer, &notes, nil, lost); !errors.Is(status, errNoAnswer) {
		t.Fatalf("the turn's own status was replaced: %v", status)
	}
	if notes.Len() == 0 {
		t.Fatal("a lost answer went unreported behind an unrelated failure")
	}
}
