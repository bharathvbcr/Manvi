package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

type fileStubHost struct {
	stubHost
	files []string
}

func (f *fileStubHost) Files(query string) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []string
	for _, p := range f.files {
		if q == "" || strings.Contains(strings.ToLower(p), q) {
			out = append(out, p)
		}
	}
	return out
}

func TestWorkCapsuleStacksTurnWorkAndPinsSafetyOutcomes(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.StackWork = true
	at := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)

	s.Append(ui.Event{Kind: ui.KindTurnStart, At: at, Text: "fix approval gate"})
	s.Append(ui.Event{Kind: ui.KindReasoning, At: at.Add(1 * time.Second), Text: "checking approval.go"})
	s.Append(ui.Event{Kind: ui.KindToolStart, At: at.Add(2 * time.Second), Tool: "read_file", Arguments: []byte(`{"path":"approval.go"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, At: at.Add(3 * time.Second), Tool: "read_file", Text: "line1\nline2\n"})
	s.Append(ui.Event{
		Kind: ui.KindToolStart, At: at.Add(4 * time.Second), Tool: "patch_file",
		Arguments: []byte(`{"path":"approval.go","target_content":"old","replacement_content":"new1\nnew2"}`),
	})
	s.Append(ui.Event{
		Kind: ui.KindToolResult, At: at.Add(5 * time.Second), Tool: "patch_file",
		Text: "patched approval.go (12 bytes)", GrantID: "G-1", GrantedBy: "human",
	})
	s.Append(ui.Event{Kind: ui.KindText, At: at.Add(6 * time.Second), Text: "Fixed approval.go."})

	rows := s.rows(th, 100)
	joined := ""
	capsuleRowIdx := -1
	for i, r := range rows {
		joined += r.line.Text() + "\n"
		if r.capsuleStart >= 0 {
			capsuleRowIdx = i
		}
	}
	if capsuleRowIdx < 0 {
		t.Fatalf("expected a stacked work capsule header row, got:\n%s", joined)
	}
	hdr := rows[capsuleRowIdx].line.Text()
	for _, want := range []string{"Worked 4s", "1 thought", "1 read", "1 edit", "+2", "-1"} {
		if !strings.Contains(hdr, want) {
			t.Fatalf("capsule header %q missing %q", hdr, want)
		}
	}
	// Safety invariant: the granted edit MUST remain visible even while collapsed.
	if !strings.Contains(joined, "cleared by G-1") {
		t.Fatalf("collapsed capsule hid a human grant:\n%s", joined)
	}
	// Routine read row should be tucked inside the collapsed capsule.
	if strings.Contains(joined, "Read approval.go") {
		t.Fatalf("collapsed capsule should hide routine reads:\n%s", joined)
	}

	// Expanding the capsule reveals the routine read row.
	s.ToggleCapsule(rows[capsuleRowIdx].capsuleStart)
	expanded := ""
	for _, r := range s.rows(th, 100) {
		expanded += r.line.Text() + "\n"
	}
	if !strings.Contains(expanded, "Read approval.go") {
		t.Fatalf("expanded capsule did not reveal Read approval.go:\n%s", expanded)
	}
}

func TestDragSelectionExtractsCleanTextAndCopiesOnRelease(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{
		SessionID: "S1",
		Event:     ui.Event{Kind: ui.KindText, Text: "alpha line one\nbeta line two"},
	})
	drawAt(a, 80, 20)

	sb := a.Current().Scroll
	lastY := sb.view.Bottom() - 1
	prevY := lastY - 1

	a.Dispatch(ActionClick{X: sb.view.X + 1, Y: prevY, Button: 1})
	a.Dispatch(ActionMotion{X: sb.view.X + 20, Y: lastY, Button: 1})
	effects := a.Dispatch(ActionRelease{X: sb.view.X + 20, Y: lastY, Button: 1})

	if len(effects) != 1 {
		t.Fatalf("expected EffectCopy on drag release, got %#v", effects)
	}
	cp, ok := effects[0].(EffectCopy)
	if !ok {
		t.Fatalf("expected EffectCopy, got %T", effects[0])
	}
	if !strings.Contains(cp.Text, "alpha line one") || !strings.Contains(cp.Text, "beta line two") {
		t.Fatalf("drag extracted %q, want both lines", cp.Text)
	}
}

func TestOSC52WrapsForTmuxAndScreen(t *testing.T) {
	raw, trunc := osc52Sequence("hello", func(string) string { return "" })
	if trunc || !strings.HasPrefix(raw, "\x1b]52;c;") || !strings.HasSuffix(raw, "\x07") {
		t.Fatalf("plain OSC 52 = %q trunc=%v", raw, trunc)
	}
	tmuxSeq, _ := osc52Sequence("hello", func(k string) string {
		if k == "TMUX" {
			return "/tmp/tmux-1000/default,123,0"
		}
		return ""
	})
	if !strings.HasPrefix(tmuxSeq, "\x1bPtmux;\x1b\x1b]52;c;") || !strings.HasSuffix(tmuxSeq, "\x07\x1b\\") {
		t.Fatalf("tmux DCS wrap = %q", tmuxSeq)
	}
	screenSeq, _ := osc52Sequence("hello", func(k string) string {
		if k == "STY" {
			return "1234.pts-0.host"
		}
		return ""
	})
	if !strings.HasPrefix(screenSeq, "\x1bP\x1b]52;c;") || !strings.HasSuffix(screenSeq, "\x07\x1b\\") {
		t.Fatalf("screen DCS wrap = %q", screenSeq)
	}
}

func TestInspectorDrawerTabsThinkingStepsDiffsAndInfo(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindReasoning, Text: "step one thought"}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Tool: "patch_file",
		Arguments: []byte(`{"path":"foo.go","target_content":"a := 1","replacement_content":"a := 2"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindToolResult, Tool: "patch_file", Text: "patched foo.go (6 bytes)"}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindReasoning, Text: "step two thought"}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindText, Text: "Done."}})

	// Toggle inspector open with Ctrl+B and draw wide enough for split pane.
	key(a, "ctrl+b")
	if !a.Current().Inspector.Open {
		t.Fatal("ctrl+b did not open inspector")
	}
	buf := drawAt(a, 160, 30)
	frame := bufferString(buf)
	if !strings.Contains(frame, "Think") || !strings.Contains(frame, "step two thought") {
		t.Fatalf("Think tab did not render latest regenerating thought:\n%s", frame)
	}

	// Switch to Diffs tab and verify synthesized diff renders.
	a.Current().Inspector.SetTab(TabDiffs)
	diffFrame := bufferString(drawAt(a, 160, 30))
	if !strings.Contains(diffFrame, "foo.go") || !strings.Contains(diffFrame, "a := 2") {
		t.Fatalf("Diffs tab did not render synthesized edit diff:\n%s", diffFrame)
	}

	// On narrow terminals (< inspectorMinWidth), drawer stays hidden so transcript is not cramped.
	narrow := drawAt(a, 80, 24)
	if !a.Current().Inspector.rect.Empty() {
		t.Fatalf("inspector should not split on narrow 80-col terminal, rect=%+v", a.Current().Inspector.rect)
	}
	_ = narrow
}

func TestSmartCopyMenuExtractsMultipleCodeBlocksAndDiffs(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{
		SessionID: "S1",
		Event: ui.Event{
			Kind: ui.KindText,
			Text: "First:\n```go\nfmt.Println(1)\n```\nSecond:\n```bash\ngo test ./...\n```",
		},
	})
	a.Dispatch(ActionEvent{
		SessionID: "S1",
		Event:     ui.Event{Kind: ui.KindReasoning, Text: "deep thought"},
	})
	ov := CopyOverlay(a.Current())
	if ov == nil {
		t.Fatal("CopyOverlay returned nil")
	}
	labels := make([]string, 0, len(ov.items))
	for _, it := range ov.items {
		labels = append(labels, it.Label)
	}
	joined := strings.Join(labels, " | ")
	for _, want := range []string{"Last reply", "Code block #1 (go)", "Code block #2 (bash)", "Latest thinking"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("CopyOverlay missing %q in %q", want, joined)
		}
	}
}

func TestAtMentionFileCompletionWorks(t *testing.T) {
	h := &fileStubHost{
		files: []string{"manvi/ui/tui/approval.go", "manvi/ui/tui/app.go", "README.md"},
	}
	a := NewApp(Dark(), h)
	a.AddSession("S1", "session one")

	for _, r := range "check @appr" {
		a.Dispatch(ActionRune{Runes: []rune{r}})
	}
	if a.overlay == nil || a.overlay.Kind != OverlayComplete {
		t.Fatalf("expected OverlayComplete for @ trigger, got %+v", a.overlay)
	}
	item, ok := a.overlay.Selected()
	if !ok || item.Value != "manvi/ui/tui/approval.go" {
		t.Fatalf("expected approval.go match, got %+v (ok=%v)", item, ok)
	}
	key(a, "tab")
	if got := a.Current().Prompt.Value(); got != "check @manvi/ui/tui/approval.go " {
		t.Fatalf("prompt after @ completion = %q", got)
	}
}

func TestMultilinePromptUpDownMovesLinesBeforeHistory(t *testing.T) {
	a, _ := newTestApp()
	p := a.Current().Prompt
	p.SetValue("older prompt")
	p.Submit()

	p.SetValue("line one\nline two")
	// Cursor is at end of line two; Up should move to line one, NOT jump to history.
	key(a, "up")
	if got := p.Value(); got != "line one\nline two" {
		t.Fatalf("Up inside multi-line draft clobbered buffer with history: %q", got)
	}
	if p.cursor > len([]rune("line one")) {
		t.Fatalf("cursor stayed on line two: %d", p.cursor)
	}
	// Up again from line one now walks history.
	key(a, "up")
	if got := p.Value(); got != "older prompt" {
		t.Fatalf("second Up from line one did not walk history: %q", got)
	}
}

func TestSyntaxHighlightingPreservesKeywordsWithStringsAndTablesRender(t *testing.T) {
	th := unicodeDark()
	hl := highlightCodeLine(`func greet() string { return "hello" }`, th, "go")
	foundFuncKeyword := false
	for _, sp := range hl {
		if sp.Text == "func" && sp.Style.Fg == th.Info && sp.Style.Has(render.Bold) {
			foundFuncKeyword = true
		}
	}
	if !foundFuncKeyword {
		t.Fatalf("keyword highlighting was suppressed on a line containing quotes: %+v", hl)
	}

	md := renderMarkdown("See *italic*, ~~old~~, and [docs](https://example.com).\n| Col A | Col B |\n|---|---|\n| 1 | 2 |", th, 80, false)
	var txt []string
	for _, l := range md {
		txt = append(txt, l.Text())
	}
	joined := strings.Join(txt, "\n")
	if !strings.Contains(joined, "Col A │ Col B") || !strings.Contains(joined, "───") {
		t.Fatalf("GFM table did not render cleanly:\n%s", joined)
	}
	if strings.Contains(joined, "https://example.com") {
		t.Fatalf("markdown link did not condense to label:\n%s", joined)
	}
}

func TestApprovalDiffExpandAndOverflowTabWarning(t *testing.T) {
	th := unicodeDark()
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, "+added line "+itoa(i))
	}
	card := NewApprovalCard(ui.Request{
		ID:        "A1",
		Rule:      "policy.file.scope",
		Severity:  "soft",
		Path:      "approval.go",
		Reason:    "out of scope",
		Grantable: true,
		Diff:      strings.Join(lines, "\n"),
	}, make(chan ui.Decision, 1))

	shortLines, _, _ := card.content(th, 80)
	card.ToggleDiff()
	expandedLines, _, _ := card.content(th, 80)
	if len(expandedLines) <= len(shortLines)+8 {
		t.Fatalf("ToggleDiff did not expand preview: before=%d after=%d", len(shortLines), len(expandedLines))
	}

	// Tab strip overflow must surface +N and hidden approval alert.
	views := make([]*AgentView, 8)
	for i := range views {
		views[i] = NewAgentView("S"+itoa(i+1), "session-long-name-"+itoa(i+1))
	}
	views[7].PushApproval(card)
	b := render.NewBuffer(70, 1)
	DrawSessionTabBar(b, b.Bounds(), th, views, 0, 0)
	row := bufferString(b)
	if !strings.Contains(row, "+") || !strings.Contains(row, th.Glyphs().Warn) {
		t.Fatalf("overflow tab strip did not surface hidden count and approval alert: %q", row)
	}
}

func TestTerminalThemeBlendsBackgroundAndPreservesStatusContrast(t *testing.T) {
	th, ok := PickThemeByName("terminal", render.TrueColor, true)
	if !ok || th.Name != "terminal" || !th.Bg.IsDefault() {
		t.Fatalf("terminal theme = %+v ok=%v, want Default Bg", th, ok)
	}
	seen := map[string]string{}
	for name, kind := range map[string]StatusKind{
		"pass": StatusPass, "warn": StatusWarn, "block": StatusBlock,
		"granted": StatusGranted, "degraded": StatusDegraded,
	} {
		seq := styleSequence(t, th.Status(kind), render.ANSI16)
		if prev, clash := seen[seq]; clash {
			t.Fatalf("terminal theme ANSI-16 clash between %s and %s: %q", prev, name, seq)
		}
		seen[seq] = name
	}
}
