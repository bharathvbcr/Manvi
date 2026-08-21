package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"manvi/credentials"
)

func renderer(t *testing.T) (*Renderer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	r := NewRenderer(&buf, nil)
	r.SetPalette(PlainPalette())
	return r, &buf
}

// TestUntrustedContentIsSanitizedOnItsWayToTheTerminal is the join between the
// sanitizer and the renderer: every field that carries content must go through
// it, and it is easy to add a field and forget.
func TestUntrustedContentIsSanitizedOnItsWayToTheTerminal(t *testing.T) {
	r, buf := renderer(t)
	hostile := "output\x1b[2J\x1b]0;pwned\x07"
	for _, e := range []Event{
		{Kind: KindText, Text: hostile},
		{Kind: KindToolResult, Text: hostile},
		{Kind: KindToolStart, Tool: hostile, Detail: hostile},
		{Kind: KindError, Text: hostile},
		{Kind: KindNotice, Text: hostile},
		{Kind: KindReasoning, Text: hostile},
		{Kind: KindTurnStart, Text: hostile},
		{Kind: KindReport, Text: hostile},
		{Kind: KindLease, Text: hostile},
		{Kind: KindApproval, Text: hostile, Rule: hostile, Severity: hostile, Path: hostile},
		{Kind: KindApprovalDone, Text: hostile},
		{Kind: KindSessionStart, Posture: hostile, Model: hostile},
		{Kind: KindPolicy, Text: hostile, Rule: hostile, Demoted: hostile, Degraded: []string{hostile}},
		{Kind: KindPolicy, Text: hostile, Rule: hostile, Severity: "hard"},
		{Kind: KindPolicy, Text: hostile, Rule: hostile, GrantID: hostile, GrantedBy: hostile},
	} {
		r.Emit(e)
	}
	out := buf.String()
	// The renderer's own colour codes are absent because the palette is plain,
	// so any ESC here came from content.
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
		t.Fatalf("an escape from content reached the terminal: %q", out)
	}
}

// TestCredentialsAreScrubbedFromRenderedText is the backstop: a provider that
// echoes a key back inside an error body must not have it land in a scrollback
// buffer.
func TestCredentialsAreScrubbedFromRenderedText(t *testing.T) {
	const key = "sk-ant-secret-value-1234567890"
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(key, "ANTHROPIC_API_KEY"))

	var buf bytes.Buffer
	r := NewRenderer(&buf, scrubber)
	r.SetPalette(PlainPalette())
	r.Emit(Event{Kind: KindError, Text: "401 from provider: invalid x-api-key " + key})

	if strings.Contains(buf.String(), key) {
		t.Fatalf("a credential reached the terminal: %q", buf.String())
	}
	if !strings.Contains(buf.String(), credentials.Redacted) {
		t.Fatalf("scrubbed without a marker: %q", buf.String())
	}
}

// TestADemotedAllowIsRenderedAsQualified: under the dev posture a write
// succeeds even though a rule fired, and a transcript that shows only the
// success teaches the operator the wrong thing.
func TestADemotedAllowIsRenderedAsQualified(t *testing.T) {
	r, buf := renderer(t)
	r.Emit(Event{
		Kind: KindToolResult, Text: "wrote src/helper.go (42 bytes)",
		Rule: "scope.unplanned", Demoted: "harness.posture=dev",
	})
	out := buf.String()
	if !strings.Contains(out, "scope.unplanned") || !strings.Contains(out, "harness.posture=dev") {
		t.Fatalf("a demoted allow was rendered as a plain success: %q", out)
	}
	if !strings.Contains(out, "would have blocked") {
		t.Fatalf("the transcript does not say what the rule would have done: %q", out)
	}
}

// TestDegradedChecksAreNamed: printing nothing is what makes an unexamined
// result look examined.
func TestDegradedChecksAreNamed(t *testing.T) {
	r, buf := renderer(t)
	r.Emit(Event{
		Kind: KindToolResult, Text: "verified",
		Degraded: []string{"diff_coverage", "secret_scan"},
	})
	for _, want := range []string{"did not run", "diff_coverage", "secret_scan"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output does not mention %q: %q", want, buf.String())
		}
	}
}

// TestDevPostureIsAnnouncedEverySession: an operator who does not see it reads
// a green run as an enforced one.
func TestDevPostureIsAnnouncedEverySession(t *testing.T) {
	r, buf := renderer(t)
	r.Emit(Event{Kind: KindSessionStart, Posture: "dev", Model: "claude-opus-5"})
	out := buf.String()
	if !strings.Contains(out, "dev posture") || !strings.Contains(out, "still block") {
		t.Fatalf("the dev posture was not explained: %q", out)
	}

	r2, buf2 := renderer(t)
	r2.Emit(Event{Kind: KindSessionStart, Posture: "strict", Model: "claude-opus-5"})
	if strings.Contains(buf2.String(), "dev posture") {
		t.Fatalf("a strict session claimed a dev posture: %q", buf2.String())
	}
}

// TestYoloPostureIsAnnouncedEverySession, and says what it did not turn off.
// The session banner is the only place an operator is told the gates are down
// before the first write lands.
func TestYoloPostureIsAnnouncedEverySession(t *testing.T) {
	r, buf := renderer(t)
	r.Emit(Event{Kind: KindSessionStart, Posture: "yolo", Model: "claude-opus-5"})
	out := buf.String()
	for _, want := range []string{"yolo posture", "hard rules included", "nothing is put to you", "repository boundary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the yolo posture banner does not mention %q: %q", want, out)
		}
	}
	if strings.Contains(out, "dev posture") {
		t.Fatalf("a yolo session described itself as dev: %q", out)
	}
}

// TestJSONSinkAndRendererConsumeTheSameEvents is the structural claim: one
// wire, two faces. A field only one of them reads is a place they can drift.
func TestJSONSinkAndRendererConsumeTheSameEvents(t *testing.T) {
	var jsonBuf, termBuf bytes.Buffer
	sinks := MultiSink{NewJSONSink(&jsonBuf, nil), func() Sink {
		r := NewRenderer(&termBuf, nil)
		r.SetPalette(PlainPalette())
		return r
	}()}

	sinks.Emit(Event{
		Kind: KindToolResult, Text: "wrote src/a.go", Tool: "devcouncil_write_file",
		Rule: "scope.unplanned", Demoted: "harness.posture=dev",
	})

	var decoded Event
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("the headless face emitted unparseable JSON: %v (%s)", err, jsonBuf.String())
	}
	if decoded.Rule != "scope.unplanned" || decoded.Demoted != "harness.posture=dev" {
		t.Fatalf("the JSON face lost the qualification: %+v", decoded)
	}
	if decoded.At.IsZero() {
		t.Fatal("the JSON face emitted no timestamp; a transcript without one cannot be correlated")
	}
	if !strings.Contains(termBuf.String(), "scope.unplanned") {
		t.Fatal("the two faces disagree about what happened")
	}
}

// TestJSONSinkScrubsCredentials: this stream is written to a file.
func TestJSONSinkScrubsCredentials(t *testing.T) {
	const key = "xai-secret-value-abcdefghij"
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(key, "XAI_API_KEY"))
	var buf bytes.Buffer
	NewJSONSink(&buf, scrubber).Emit(Event{Kind: KindError, Text: "auth failed for " + key})
	if strings.Contains(buf.String(), key) {
		t.Fatalf("a credential was written to the JSON transcript: %s", buf.String())
	}
}

// TestStreamedTextDoesNotRunIntoTheNextEvent.
func TestStreamedTextDoesNotRunIntoTheNextEvent(t *testing.T) {
	r, buf := renderer(t)
	r.Emit(Event{Kind: KindText, Text: "Reading "})
	r.Emit(Event{Kind: KindText, Text: "the file."})
	r.Emit(Event{Kind: KindToolStart, Tool: "devcouncil_read_file"})
	out := buf.String()
	if !strings.Contains(out, "Reading the file.\n") {
		t.Fatalf("streamed deltas were not joined, or the line was not closed: %q", out)
	}
}

// TestShouldColorRefusesAPipe: escape codes in a redirected log make it
// unreadable, and this is the check that keeps them out.
func TestShouldColorRefusesAPipe(t *testing.T) {
	if ShouldColor(&bytes.Buffer{}) {
		t.Fatal("colour was enabled for a non-terminal writer")
	}
	t.Setenv("NO_COLOR", "1")
	if ShouldColor(&bytes.Buffer{}) {
		t.Fatal("NO_COLOR was ignored")
	}
}

// --- approval seam ---

// TestAnUnanswerablePromptIsARefusal is the one failure mode an approval seam
// must not have: treating a question nobody answered as consent.
func TestAnUnanswerablePromptIsARefusal(t *testing.T) {
	req := Request{Rule: "scope.unplanned", Path: "src/helper.go", Grantable: true, Severity: "soft"}

	t.Run("closed input", func(t *testing.T) {
		p := NewPrompter(strings.NewReader(""), &bytes.Buffer{}, Discard)
		d, err := p.Approve(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("a closed input was read as approval")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		p := NewPrompter(blockingReader{}, &bytes.Buffer{}, Discard)
		p.Timeout = 100 * time.Millisecond
		d, err := p.Approve(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("a timed-out prompt was read as approval")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := NewPrompter(blockingReader{}, &bytes.Buffer{}, Discard)
		d, err := p.Approve(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("a cancelled prompt was read as approval")
		}
	})
}

// TestApprovalRequiresARecordedReason: a grant with a manufactured reason reads
// in a later review exactly like one with a real reason.
func TestApprovalRequiresARecordedReason(t *testing.T) {
	p := NewPrompter(strings.NewReader("y\n\n"), &bytes.Buffer{}, Discard)
	d, err := p.Approve(context.Background(),
		Request{Rule: "scope.unplanned", Path: "src/a.go", Grantable: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Fatal("an approval with no reason was issued")
	}

	p2 := NewPrompter(strings.NewReader("y\nthe fix needs a helper\n"), &bytes.Buffer{}, Discard)
	d2, err := p2.Approve(context.Background(),
		Request{Rule: "scope.unplanned", Path: "src/a.go", Grantable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d2.Allow || d2.Reason != "the fix needs a helper" || d2.By != "human" {
		t.Fatalf("decision = %+v", d2)
	}
}

// TestAnUngrantableRuleIsNotEvenAsked: presenting a question with only one
// possible answer trains an operator to answer without reading.
func TestAnUngrantableRuleIsNotEvenAsked(t *testing.T) {
	var out bytes.Buffer
	p := NewPrompter(strings.NewReader("y\nplease\n"), &out, Discard)
	d, err := p.Approve(context.Background(),
		Request{Rule: "path.secret", Severity: "hard", Path: ".env", Grantable: false})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Fatal("a hard rule was cleared by a prompt")
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("a question was asked that had only one answer: %q", out.String())
	}
}

// TestDenyAllIsTheUnattendedDefault: an approval nobody is present to give must
// not be granted by absence.
func TestDenyAllIsTheUnattendedDefault(t *testing.T) {
	d, err := DenyAll{}.Approve(context.Background(),
		Request{Rule: "scope.unplanned", Grantable: true})
	if err != nil || d.Allow {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
}

// TestAutoApproverClearsOnlyItsListedRules, and never a hard one even if
// misconfigured to name it.
func TestAutoApproverClearsOnlyItsListedRules(t *testing.T) {
	a := AutoApprover{
		Rules:  map[string]bool{"scope.unplanned": true, "path.secret": true},
		Reason: "unattended migration run, approved in the ticket",
	}
	if d, _ := a.Approve(context.Background(),
		Request{Rule: "scope.unplanned", Grantable: true}); !d.Allow {
		t.Fatal("a listed rule was not cleared")
	}
	if d, _ := a.Approve(context.Background(),
		Request{Rule: "scope.read_only", Grantable: true}); d.Allow {
		t.Fatal("an unlisted rule was cleared")
	}
	// Grantability is checked before the list, so naming a hard rule in the
	// configuration cannot clear one.
	if d, _ := a.Approve(context.Background(),
		Request{Rule: "path.secret", Grantable: false}); d.Allow {
		t.Fatal("a hard rule was cleared by a misconfigured auto-approver")
	}
	if _, err := (AutoApprover{Rules: map[string]bool{"x": true}}).Approve(
		context.Background(), Request{Rule: "x", Grantable: true}); err == nil {
		t.Fatal("an auto-approver with no recorded reason was allowed to grant")
	}
}

// TestTheApprovalAndItsAnswerBothReachTheTranscript: an answer with no
// recorded question cannot be audited.
func TestTheApprovalAndItsAnswerBothReachTheTranscript(t *testing.T) {
	var events []Event
	sink := SinkFunc(func(e Event) { events = append(events, e) })
	p := NewPrompter(strings.NewReader("y\nneeded for the fix\n"), &bytes.Buffer{}, sink)
	if _, err := p.Approve(context.Background(),
		Request{Rule: "scope.unplanned", Path: "src/a.go", Grantable: true}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != KindApproval || events[1].Kind != KindApprovalDone {
		t.Fatalf("events = %+v, want the request and the decision", events)
	}
	if events[0].ApprovalID == "" || events[0].ApprovalID != events[1].ApprovalID {
		t.Fatalf("the question and answer are not linked: %+v", events)
	}
	if !strings.Contains(events[1].Text, "needed for the fix") {
		t.Fatalf("the recorded reason was lost: %+v", events[1])
	}
}

// blockingReader never returns, standing in for a terminal nobody is at.
type blockingReader struct{}

func (blockingReader) Read(p []byte) (int, error) { select {} }
