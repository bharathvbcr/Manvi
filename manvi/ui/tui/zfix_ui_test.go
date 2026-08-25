package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"manvi/credentials"
	"manvi/ui"
	"manvi/ui/render"
)

// fakeKey is the value these tests use wherever a credential is wanted. It is
// not a key, it has never been a key, and nothing here reads a real one from
// anywhere.
const fakeKey = "sk-ant-FAKE-TESTVALUE-1234567890abcdef"

// paintFrame runs one action through the loop's own entry point and returns the
// exact bytes the painter would have written to the tty.
//
// A Runner is assembled here rather than through New because New refuses to
// build one without a real terminal — which is precisely why the full-screen
// face had no test that reached the wire. Every existing test stops at
// App.Dispatch, and App is downstream of the seam that was missing. The fields
// set below are the ones apply and draw touch, so this is the production path
// with the tty replaced by a buffer.
func paintFrame(t *testing.T, scrubber *credentials.Scrubber, act Action) string {
	t.Helper()
	const w, h = 100, 30

	host := &stubHost{}
	app := NewApp(Dark(), host)
	if scrubber == nil {
		scrubber = credentials.NewScrubber()
	}
	r := &Runner{
		cfg:     Config{Host: host, Scrubber: scrubber},
		app:     app,
		actions: make(chan Action, 64),
		cancels: map[string]context.CancelFunc{},
		done:    make(chan struct{}),
	}
	ctx := context.Background()
	r.apply(ctx, ActionResize{W: w, H: h})
	app.AddSession("S1", "session one")
	r.apply(ctx, act)

	painter := render.NewPainter(w, h, render.NoColor)
	caret := app.Draw(painter.Buffer())
	cursor := render.Cursor{}
	if caret.W > 0 {
		cursor = render.Cursor{X: caret.X, Y: caret.Y, Visible: true}
	}
	painter.Invalidate()
	var out bytes.Buffer
	if err := painter.Flush(&out, cursor); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out.String()
}

// neverPainted are sequences this painter provably does not emit, so finding one
// in a frame means content put it there.
//
// The painter's whole vocabulary is cursor addressing, erase-to-end-of-line, the
// cursor show/hide pair, and SGR — it has no reason to clear a screen, open a
// hyperlink, write a clipboard, or start a device-control string. Each frame is
// checked against a benign baseline first, so an entry here cannot pass by
// naming something the painter could never have produced anyway.
var neverPainted = []string{
	"\x1b[2J",  // clear screen
	"\x1b[2K",  // erase line
	"\x1b]8;;", // OSC 8 hyperlink: the visible text need not match its target
	"\x1b]52;", // OSC 52 clipboard write
	"\x1bP",    // DCS
	"\x1b\\",   // string terminator
	"\x1b[31m", // an SGR colour, which a NoColor profile never writes
	"\x00",     // NUL
	"\u0090",   // C1 DCS: the eight-bit form, with no ESC byte in front of it
	"\u009b",   // C1 CSI
	"\u202e",   // bidi override: reorders what a reviewer reads
	"\r",       // a bare carriage return overwrites the line already drawn
	"\x7f",     // DEL
}

// assertFrameIsSanitized is the assertion that would have caught this defect.
//
// It makes two statements. The first is the strong one: the frame painted from
// untrusted text is byte-identical to the frame painted from that same text
// already run through Sanitize. Sanitize is idempotent, so the two can only
// match when the seam applied it — and this needs no list of sequences kept up
// to date, which is what makes it hold for whatever the next payload turns out
// to be.
//
// The second is direct and readable: none of the sequences the painter never
// emits appears in the frame. It is here because the first compares a frame
// against a frame, and a bug that damaged both renderings equally would satisfy
// it while painting something dangerous.
func assertFrameIsSanitized(t *testing.T, name string, mk func(string) Action, payload string) {
	t.Helper()

	// A field this frame does not draw cannot be examined here, and a subtest
	// that examines nothing must not be counted as one that examined something
	// and found it clean. So the field is proved visible first, with a benign
	// marker, and named in the output when it is not. CleanEvent's coverage of
	// every field, drawn or not, is asserted structurally in package ui.
	const marker = "zqxmarkerzqx"
	if control := paintFrame(t, nil, mk(marker)); !strings.Contains(control, marker) {
		t.Skipf("%s: this field is not drawn in the frame this action produces, so no frame assertion ran for it", name)
	}

	got := paintFrame(t, nil, mk(payload))
	want := paintFrame(t, nil, mk(ui.Sanitize(payload)))
	if got != want {
		t.Errorf("%s: the frame from untrusted text differs from the frame from its sanitized form\n got: %q\nwant: %q",
			name, got, want)
	}
	baseline := paintFrame(t, nil, mk("harmless text"))
	for _, seq := range neverPainted {
		if strings.Contains(baseline, seq) {
			t.Fatalf("%s: the painter emits %q on its own, so its absence proves nothing", name, seq)
		}
		if strings.Contains(got, seq) {
			t.Errorf("%s: the frame carries %q, which only content could have put there\nframe: %q",
				name, seq, got)
		}
	}
}

// payloads is the attack corpus: each is something a terminal acts on rather
// than displays.
var payloads = map[string]string{
	"clear screen":           "before \x1b[2J after",
	"cursor home":            "before \x1b[1;1H after",
	"erase line":             "before \x1b[2K after",
	"cursor into the prompt": "before \x1b[1;5H after",
	"sgr colour":             "a\x1b[31mX",
	"osc 8 hyperlink":        "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07",
	"osc 52 clipboard":       "\x1b]52;c;bWFsaWNl\x07",
	"dcs":                    "\x1bPq#0;2;0;0;0#0~~@\x1b\\",
	"c1 controls":            "before \u0090 and \u009b2J after",
	"lone carriage return":   "visible\rhidden",
	"nul":                    "a\x00b",
	"bidi override":          "start \u202e dne",
	"del":                    "a\x7fb",
}

// TestModelTextNeverReachesTheFrameAsAnEscapeSequence is defect 1.
//
// ui/tui contained no call to Sanitize at all. An event's Text went from the
// harness's sink to the scrollback to the buffer unexamined; RuneWidth answers 0
// for ESC, SetRune hung it off the previous cell's combining marks, and the
// painter writes those marks to the tty verbatim — reassembling a working
// control sequence out of a frame that believed it held plain characters.
func TestModelTextNeverReachesTheFrameAsAnEscapeSequence(t *testing.T) {
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			assertFrameIsSanitized(t, name, func(s string) Action {
				return ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindText, Text: s}}
			}, payload)
		})
	}
}

// TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame widens defect 1 from
// the one field that was noticed to the class.
//
// Text was not special. Detail, the tool name, Path, Rule, the agent
// attribution, the degraded and weakened lists and a tool call's decoded
// arguments are all written by something other than this harness, and every one
// of them is drawn.
func TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame(t *testing.T) {
	const payload = "x\x1b[2Jy"
	event := func(build func(string) ui.Event) func(string) Action {
		return func(s string) Action {
			return ActionEvent{SessionID: "S1", Event: build(s)}
		}
	}
	fields := map[string]func(string) Action{
		"text":   event(func(s string) ui.Event { return ui.Event{Kind: ui.KindText, Text: s} }),
		"detail": event(func(s string) ui.Event { return ui.Event{Kind: ui.KindToolStart, Tool: "read", Detail: s} }),
		"tool":   event(func(s string) ui.Event { return ui.Event{Kind: ui.KindToolStart, Tool: s} }),
		"result": event(func(s string) ui.Event { return ui.Event{Kind: ui.KindToolResult, Text: s} }),
		"rule": event(func(s string) ui.Event {
			return ui.Event{Kind: ui.KindPolicy, Text: "blocked", Rule: s, Severity: "soft"}
		}),
		"path": event(func(s string) ui.Event {
			return ui.Event{Kind: ui.KindPolicy, Text: "blocked", Path: s, Severity: "soft"}
		}),
		"error":  event(func(s string) ui.Event { return ui.Event{Kind: ui.KindError, Text: s} }),
		"notice": event(func(s string) ui.Event { return ui.Event{Kind: ui.KindNotice, Text: s} }),
		"agent": event(func(s string) ui.Event {
			return ui.Event{Kind: ui.KindText, Agent: s, Text: "the child said something"}
		}),
		"degraded": event(func(s string) ui.Event {
			return ui.Event{Kind: ui.KindPolicy, Text: "allowed", Degraded: []string{s}}
		}),
		"weakened": event(func(s string) ui.Event {
			return ui.Event{Kind: ui.KindPolicy, Text: "allowed", Weakened: []string{s}}
		}),
		// Arguments are JSON, and the escape sits inside a string value where it
		// is perfectly valid JSON. Decoding is what makes it dangerous, and
		// decoding is what the tool banner does.
		"arguments": event(func(s string) ui.Event {
			raw, err := json.Marshal(map[string]string{"path": s})
			if err != nil {
				panic(err)
			}
			return ui.Event{Kind: ui.KindToolStart, Tool: "write", Arguments: raw}
		}),
		// Captured stderr is a subprocess's own bytes, not the harness's.
		"notice action": func(s string) Action { return ActionNotice{SessionID: "S1", Text: s} },
		// A session title comes out of the store, which sanitize.go names as
		// untrusted for exactly this reason.
		"session title": func(s string) Action { return ActionSessionAdded{ID: "S2", Title: s} },
		// The error a turn ended with is assembled by a provider.
		"turn error": func(s string) Action {
			return ActionTurnEnded{SessionID: "S1", Err: errors.New(s)}
		},
	}
	for name, mk := range fields {
		t.Run(name, func(t *testing.T) { assertFrameIsSanitized(t, name, mk, payload) })
	}
}

// TestTheApprovalModalCannotBeRepaintedByTheModel is defect 2.
//
// Request.Path is the model-composed shell command line whenever the subject is
// a command, and Reason is model-authored wherever a tool asks a question. Both
// are drawn inside the card, and the card is the human-in-the-loop control: an
// escape sequence there does not corrupt a display, it changes the question a
// human believes they are answering. The CLI approver neutralised the identical
// string; this one did not.
func TestTheApprovalModalCannotBeRepaintedByTheModel(t *testing.T) {
	mk := func(s string) Action {
		return ActionApprovalRequest{
			SessionID: "S1",
			Reply:     make(chan ui.Decision, 1),
			Request: ui.Request{
				ID:        "A1",
				Rule:      "cmd.dangerous" + s,
				Severity:  "soft",
				Subject:   "command",
				Path:      "rm -rf /" + s,
				Reason:    "just running the tests" + s,
				TaskID:    "T1" + s,
				Grantable: true,
			},
		}
	}
	assertFrameIsSanitized(t, "approval card", mk, "\x1b[2K\x1b[1;5Hgit status")

	// And the card really is on screen, so the assertion above is not passing by
	// having drawn nothing at all.
	frame := paintFrame(t, nil, mk(""))
	if !strings.Contains(frame, "approval required") {
		t.Fatalf("no approval card was drawn:\n%q", frame)
	}
}

// TestAQuestionsChoicesCannotRepaintTheRowBeingChosen.
//
// A question's options are drawn as the rows the operator moves between, so an
// escape in one repaints the very control being operated.
func TestAQuestionsChoicesCannotRepaintTheRowBeingChosen(t *testing.T) {
	mk := func(s string) Action {
		return ActionApprovalRequest{
			SessionID: "S1",
			Reply:     make(chan ui.Decision, 1),
			Request: ui.Request{
				ID: "A1", Grantable: true, Reason: "which one?",
				Choices: []string{"safe option", "danger" + s},
			},
		}
	}
	assertFrameIsSanitized(t, "question choices", mk, "\x1b[2J\x1b[1;1H")
}

// TestTheApprovalCardAnswersWithTheOptionTheOperatorWasShown pins the
// consequence of cleaning Choices: a decision carries the cleaned option, not a
// hidden original. An operator must not be able to pick a row that reads one way
// and answers another.
func TestTheApprovalCardAnswersWithTheOptionTheOperatorWasShown(t *testing.T) {
	host := &stubHost{}
	app := NewApp(Dark(), host)
	r := &Runner{
		cfg: Config{Host: host, Scrubber: credentials.NewScrubber()}, app: app,
		actions: make(chan Action, 8), cancels: map[string]context.CancelFunc{}, done: make(chan struct{}),
	}
	ctx := context.Background()
	r.apply(ctx, ActionResize{W: 100, H: 30})
	app.AddSession("S1", "session one")
	r.apply(ctx, ActionApprovalRequest{
		SessionID: "S1", Reply: make(chan ui.Decision, 1),
		Request: ui.Request{ID: "A1", Grantable: true, Reason: "which one?",
			Choices: []string{"yes\x1b[2Kno"}},
	})

	card := app.Current().Approval()
	if card == nil {
		t.Fatal("no approval card")
	}
	for _, opt := range card.options() {
		if strings.ContainsRune(opt, 0x1b) {
			t.Fatalf("an option still carries a raw escape: %q", opt)
		}
	}
	if got := card.Request.Choices[0]; strings.ContainsRune(got, 0x1b) {
		t.Fatalf("the choice a decision carries back is uncleaned: %q", got)
	}
}

// TestCredentialsNeverReachTheFrame is defect 3.
//
// manvi run, manvi watch and manvi probe each wired a scrubber. The full-screen
// face wired none, and it is the face where a leaked key lands in a human's
// scrollback. The shortest path is the one named in the report: a provider fails
// with the key echoed back in the error body, the turn ends, and the app writes
// err.Error() straight into the transcript.
func TestCredentialsNeverReachTheFrame(t *testing.T) {
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(fakeKey, "ANTHROPIC_API_KEY"))

	cases := map[string]Action{
		"turn error": ActionTurnEnded{
			SessionID: "S1",
			Err:       errors.New(`401 from provider: {"api_key":"` + fakeKey + `"}`),
		},
		"error event": ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindError, Text: "auth failed for " + fakeKey,
		}},
		"tool arguments": ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindToolStart, Tool: "http",
			Arguments: json.RawMessage(`{"header":"Bearer ` + fakeKey + `"}`),
		}},
		"captured stderr": ActionNotice{SessionID: "S1", Text: "env dump: KEY=" + fakeKey},
		"session title":   ActionSessionAdded{ID: "S2", Title: "task " + fakeKey},
		"policy path": ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindPolicy, Text: "blocked", Severity: "soft",
			Rule: "path.credential", Path: "/tmp/" + fakeKey,
		}},
		"approval reason": ActionApprovalRequest{
			SessionID: "S1", Reply: make(chan ui.Decision, 1),
			Request: ui.Request{ID: "A1", Grantable: true, Severity: "soft",
				Rule: "cmd.network", Subject: "command",
				Path:   "curl -H 'Authorization: Bearer " + fakeKey + "'",
				Reason: "I need to call the API with " + fakeKey},
		},
	}
	for name, act := range cases {
		t.Run(name, func(t *testing.T) {
			if frame := paintFrame(t, scrubber, act); strings.Contains(frame, fakeKey) {
				t.Fatalf("%s painted the credential into the frame:\n%q", name, frame)
			}
		})
	}
}

// TestASecretSplitAcrossTwoFieldsIsRemovedFromEachOfThem.
//
// The scrubber matches whole values, so one repeated across several fields has
// to go from all of them — and a value that only ever appears cut in half is not
// a match, which is why the order of scrub-then-truncate matters wherever a
// bound is applied. That second half is stated here rather than assumed, so
// nobody reads the first as a promise about it.
func TestASecretSplitAcrossTwoFieldsIsRemovedFromEachOfThem(t *testing.T) {
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(fakeKey, "ANTHROPIC_API_KEY"))

	frame := paintFrame(t, scrubber, ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindPolicy, Text: "blocked writing " + fakeKey, Severity: "soft",
		Path: "/tmp/" + fakeKey, Rule: "path.credential",
		Degraded: []string{"neighbour check: " + fakeKey},
		Weakened: []string{"policy.file.mode: " + fakeKey},
	}})
	if strings.Contains(frame, fakeKey) {
		t.Fatalf("a credential repeated across fields survived in the frame:\n%q", frame)
	}

	half := fakeKey[:len(fakeKey)/2]
	if scrubber.Clean(half) != half {
		t.Fatal("half a watched value was treated as a match, which is not what Clean does")
	}
}

// TestCleaningAnErrorKeepsItsIdentity: the turn's error is rewritten so its text
// is safe, and errors.Is must still reach the original. A display concern is not
// allowed to break a caller matching on a sentinel.
func TestCleaningAnErrorKeepsItsIdentity(t *testing.T) {
	sanitize := func(s string) string { return ui.Sanitize(s) }

	sentinel := errors.New("sentinel")
	cleaned := cleanError(testError{err: sentinel, text: "boom \x1b[2J"}, sanitize)
	if strings.ContainsRune(cleaned.Error(), 0x1b) {
		t.Fatalf("the cleaned error still carries an escape: %q", cleaned.Error())
	}
	if !errors.Is(cleaned, sentinel) {
		t.Fatal("cleaning the text lost the error's identity")
	}
	plain := errors.New("nothing to clean")
	if cleanError(plain, sanitize) != plain {
		t.Fatal("an error needing no cleaning was replaced anyway")
	}
	if cleanError(nil, sanitize) != nil {
		t.Fatal("a nil error became non-nil")
	}
}

// testError has a message that needs cleaning and an identity that must survive
// it.
type testError struct {
	err  error
	text string
}

func (e testError) Error() string { return e.text }
func (e testError) Unwrap() error { return e.err }

// TestCleanActionLeavesTheOperatorsOwnInputAlone.
//
// The seam cleans third-party content. A keystroke and a pointer position are
// not that, and rewriting what someone typed on its way to the composer would
// change the message the harness sends on their behalf.
func TestCleanActionLeavesTheOperatorsOwnInputAlone(t *testing.T) {
	clean := func(string) string { return "REWRITTEN" }
	for _, act := range []Action{
		ActionRune{Runes: []rune("hi")},
		ActionPaste{Text: "pasted"},
		ActionKey{Binding: "ctrl+c"},
		ActionScroll{Delta: 3},
		ActionResize{W: 10, H: 10},
	} {
		if got := cleanAction(act, clean); !reflect.DeepEqual(got, act) {
			t.Errorf("cleanAction rewrote %#v into %#v", act, got)
		}
	}
}
