package tui

import (
	"bytes"
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

func rectString(b *render.Buffer, r render.Rect) string {
	var out []string
	for y := r.Y; y < r.Bottom(); y++ {
		var row strings.Builder
		for x := r.X; x < r.Right(); x++ {
			c := b.Cell(x, y)
			if c.R == 0 {
				continue
			}
			row.WriteRune(c.R)
		}
		out = append(out, row.String())
	}
	return strings.Join(out, "\n")
}

func TestInspectorDiffsLinksHonestyAndSanitization(t *testing.T) {
	a, _ := newTestApp()
	// 1. Granted edit with absolute path: must carry granted glyph + [granted] and OSC 8 file:// link.
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Tool: "patch_file",
		Arguments: []byte(`{"path":"/repo/granted file.go","target_content":"a := 1","replacement_content":"a := 2"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Tool: "patch_file", Text: "patched granted file.go",
		GrantID: "G-42", GrantedBy: "human",
	}})
	// 2. Clean edit with hostile control char in path: must strip controls and refuse OSC 8 link.
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Tool: "write_file",
		Arguments: []byte(`{"path":"/repo/evil\u001b]52;c;YQ==\u0007.go","content":"package main\n"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Tool: "write_file", Text: "wrote file",
	}})
	// 3. Pending edit (no ToolResult yet): must NOT show as applied "M".
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Tool: "patch_file",
		Arguments: []byte(`{"path":"pending.go","target_content":"old","replacement_content":"new"}`),
	}})

	// editsOf (used by CopyOverlay/keys) must exclude the pending edit.
	applied := editsOf(a.Current().Scroll.Entries())
	if len(applied) != 2 {
		t.Fatalf("editsOf returned %d edits, want 2 (excluding pending)", len(applied))
	}

	a.Current().Inspector.Toggle()
	a.Current().Inspector.SetTab(TabDiffs)
	p := render.NewPainter(160, 32, render.TrueColor)
	a.Draw(p.Buffer())
	var wire bytes.Buffer
	if err := p.Flush(&wire, render.Cursor{}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	wireStr := wire.String()
	frame := rectString(p.Buffer(), a.Current().Inspector.rect)

	if !strings.Contains(frame, "pending.go (pending)") {
		t.Fatalf("pending edit not marked as pending:\n%s", frame)
	}
	if strings.Contains(frame, "M pending.go") {
		t.Fatalf("pending edit must never be labeled as applied M:\n%s", frame)
	}
	if !strings.Contains(frame, "[granted]") {
		t.Fatalf("granted edit missing [granted] qualifier:\n%s", frame)
	}
	// Absolute path must emit percent-encoded OSC 8 file:/// link.
	if !strings.Contains(wireStr, "\x1b]8;;file:///repo/granted%20file.go\x1b\\") {
		t.Fatalf("Diffs tab did not emit percent-encoded OSC 8 file:// link:\n%q", wireStr)
	}
	// Hostile path must NOT inject OSC 52 or emit an OSC 8 link for that path.
	if strings.Contains(wireStr, "\x1b]52;") {
		t.Fatalf("hostile path injected OSC 52 into painter output: %q", wireStr)
	}

	for _, bad := range []string{
		"/repo/bad\x1bpath.go",
		"file://host/share",
		"file:///etc/passwd",
		"https://example.com/a.go",
		"javascript:alert(1)",
		"//host/share/a.go",
		"relative/foo.go",
	} {
		if got := fileLinkURL(bad); got != "" {
			t.Fatalf("fileLinkURL(%q) = %q, want empty (refused)", bad, got)
		}
	}
	if got := fileLinkURL("/repo/sub/../clean file.go"); got != "file:///repo/clean%20file.go" {
		t.Fatalf("fileLinkURL clean absolute = %q, want file:///repo/clean%%20file.go", got)
	}
}

func TestInspectorStepsCollapsibleSubagentsPreservePinnedOutcomes(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindTurnStart, Text: "inspect subagents"}})
	// Subagent "reader": 1 routine read + 1 failed/error tool result.
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Agent: "reader", Tool: "read_file",
		Arguments: []byte(`{"path":"alpha.go"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Agent: "reader", Tool: "read_file", Text: "alpha-ok",
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Agent: "reader", Tool: "read_file",
		Arguments: []byte(`{"path":"missing.go"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Agent: "reader", Tool: "read_file", IsError: true, Text: "not found",
	}})
	// Subagent "builder": 1 clean edit.
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolStart, Agent: "builder", Tool: "write_file",
		Arguments: []byte(`{"path":"beta.go","content":"ok\n"}`),
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Agent: "builder", Tool: "write_file", Text: "wrote beta.go",
	}})

	ins := a.Current().Inspector
	ins.Toggle()
	ins.SetTab(TabSteps)
	buf := drawAt(a, 160, 32)
	frame := rectString(buf, ins.rect)

	if !strings.Contains(frame, "[reader]") || !strings.Contains(frame, "alpha-ok") || !strings.Contains(frame, "not found") {
		t.Fatalf("expanded Steps tab missing reader events:\n%s", frame)
	}
	if !strings.Contains(frame, "[builder]") || !strings.Contains(frame, "wrote beta.go") {
		t.Fatalf("expanded Steps tab missing builder events:\n%s", frame)
	}
	if !strings.Contains(frame, "1 failed") {
		t.Fatalf("subagent header missing failure count:\n%s", frame)
	}

	// Find recorded hit rect for [reader] and click it via App dispatch.
	var readerRect render.Rect
	for _, ar := range ins.agentRects {
		if ar.agent == "reader" {
			readerRect = ar.rect
			break
		}
	}
	if readerRect.Empty() {
		t.Fatalf("no hit rect recorded for [reader] subagent header: %+v", ins.agentRects)
	}
	a.Dispatch(ActionClick{X: readerRect.X + 1, Y: readerRect.Y, Button: 1})
	collapsed := rectString(drawAt(a, 160, 32), ins.rect)

	// Routine read alpha-ok should now be hidden in the Inspector, while builder
	// remains expanded, AND the failed "not found" row MUST remain pinned visibly.
	if strings.Contains(collapsed, "alpha-ok") {
		t.Fatalf("collapsed [reader] section still showed routine alpha-ok:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "not found") {
		t.Fatalf("collapsed [reader] section hid failed row:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "wrote beta.go") {
		t.Fatalf("collapsing [reader] affected [builder] section:\n%s", collapsed)
	}

	// Click again to re-expand [reader].
	a.Dispatch(ActionClick{X: readerRect.X + 1, Y: readerRect.Y, Button: 1})
	reexpanded := rectString(drawAt(a, 160, 32), ins.rect)
	if !strings.Contains(reexpanded, "alpha-ok") {
		t.Fatalf("second click did not re-expand [reader]:\n%s", reexpanded)
	}
}

func TestWideDashboardShowsSplitLivePreviewPaneAndFallsBackNarrow(t *testing.T) {
	a := NewApp(unicodeDark(), &stubHost{})
	v1 := a.AddSession("S1", "session one")
	v1.Title = "alpha-session"
	v1.Status.Posture = "strict"
	v1.Status.Model = "claude-sonnet-4"
	v1.Status.TaskID = "T-101"
	v1.Status.Grants = 2
	for i := 1; i <= 12; i++ {
		v1.Apply(ui.Event{Kind: ui.KindNotice, Text: "transcript row " + itoa(i)})
	}

	a.AddSession("S2", "blocked-session")
	v2 := a.views[1]
	v2.Status.Posture = "dev"
	v2.Status.Model = "gpt-5"
	a.Dispatch(ActionApprovalRequest{
		SessionID: "S2",
		Request: ui.Request{
			ID:       "REQ-9",
			Rule:     "policy.shell.exec",
			Severity: "soft",
			Subject:  "command",
			Path:     "rm -rf build/",
			Reason:   "clean build directory",
		},
		Reply: make(chan ui.Decision, 1),
	})
	// Switch back to S1 (unblocked) so Ctrl+G is not gated by S2's modal approval card.
	a.setActive(0)

	key(a, "ctrl+g")
	if a.mode != ModeDashboard {
		t.Fatal("ctrl+g did not open dashboard")
	}

	// Highlight session 0 (unblocked, 12 transcript rows -> last 8 shown).
	a.dashboard.Select(0)
	wideFrame := bufferString(drawAt(a, 132, 36))
	for _, want := range []string{"│", "Posture", "strict", "claude-sonnet-4", "T-101", "2 grant(s)", "Recent Transcript", "transcript row 12", "transcript row 5"} {
		if !strings.Contains(wideFrame, want) {
			t.Fatalf("wide dashboard missing %q:\n%s", want, wideFrame)
		}
	}
	if strings.Contains(wideFrame, "transcript row 1\n") || strings.Contains(wideFrame, "transcript row 4") {
		t.Fatalf("wide dashboard preview showed more than last 8 transcript rows:\n%s", wideFrame)
	}

	// Highlight session 1 (blocked -> pending approval details shown).
	a.dashboard.Select(1)
	blockedFrame := bufferString(drawAt(a, 132, 36))
	for _, want := range []string{"Pending Approval", "policy.shell.exec", "Command", "rm -rf build/", "clean build directory"} {
		if !strings.Contains(blockedFrame, want) {
			t.Fatalf("wide dashboard blocked preview missing %q:\n%s", want, blockedFrame)
		}
	}

	// Narrow width (< 116) falls back to single column without preview pane.
	narrowFrame := bufferString(drawAt(a, 100, 30))
	if strings.Contains(narrowFrame, "Recent Transcript") || strings.Contains(narrowFrame, "Pending Approval") {
		t.Fatalf("narrow dashboard (<116) should not render split preview pane:\n%s", narrowFrame)
	}
}
