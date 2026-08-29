package tui

import (
	"strings"
	"testing"
	"time"

	"manvi/ui"
	"manvi/ui/render"
)

func bufferString(b *render.Buffer) string {
	var sb strings.Builder
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			sb.WriteRune(b.Cell(x, y).R)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestTranscriptSearch(t *testing.T) {
	th := Dark()
	sb := NewScrollback()
	sb.Append(ui.Event{Kind: ui.KindNotice, Text: "first entry with alpha token"})
	sb.Append(ui.Event{Kind: ui.KindNotice, Text: "second entry with beta token"})
	sb.Append(ui.Event{Kind: ui.KindNotice, Text: "third entry with alpha token again"})

	// Fold the third entry
	sb.entries[2].Foldable = true
	sb.entries[2].Folded = true

	ok := sb.SetSearch("alpha", th)
	if !ok {
		t.Fatalf("expected search match for 'alpha'")
	}
	if sb.MatchCount() != 2 {
		t.Fatalf("expected 2 matches, got %d", sb.MatchCount())
	}
	if sb.Selected() != sb.entries[0] {
		t.Fatalf("expected first entry selected, got %v", sb.Selected())
	}

	// NextMatch should jump to entry 2 and unfold it
	if !sb.NextMatch(th) {
		t.Fatalf("expected NextMatch to succeed")
	}
	if sb.Selected() != sb.entries[2] {
		t.Fatalf("expected third entry selected, got %v", sb.Selected())
	}
	if sb.entries[2].Folded {
		t.Fatalf("expected matching folded entry to auto-unfold")
	}

	// PrevMatch jumps back
	if !sb.PrevMatch(th) {
		t.Fatalf("expected PrevMatch to succeed")
	}
	if sb.Selected() != sb.entries[0] {
		t.Fatalf("expected first entry selected after PrevMatch")
	}

	status := sb.SearchStatus()
	if !strings.Contains(status, "match 1 of 2") {
		t.Fatalf("unexpected SearchStatus: %q", status)
	}

	sb.ClearSearch()
	if sb.SearchQuery() != "" || sb.MatchCount() != 0 {
		t.Fatalf("expected search cleared")
	}
}

func TestContextPressureGauge(t *testing.T) {
	th := Dark()
	th.Unicode = true
	st := Status{
		Posture:     "strict",
		ContextUsed: 6000,
		ContextMax:  10000,
	}
	b := render.NewBuffer(120, 1)
	DrawStatusBar(b, render.Rect{X: 0, Y: 0, W: 120, H: 1}, th, st, 0, nil, false)
	out := bufferString(b)
	if !strings.Contains(out, "ctx ▓▓▓░░ 60%") {
		t.Fatalf("expected context gauge in status bar, got: %q", out)
	}
}

func TestStallDetection(t *testing.T) {
	th := Dark()
	st := Status{
		Posture:   "strict",
		Busy:      true,
		BusyLabel: "run_command",
		StallSecs: 45,
	}
	b := render.NewBuffer(120, 1)
	DrawStatusBar(b, render.Rect{X: 0, Y: 0, W: 120, H: 1}, th, st, 0, nil, false)
	out := bufferString(b)
	if !strings.Contains(out, "run_command (no progress 45s)") {
		t.Fatalf("expected stall indicator in status bar, got: %q", out)
	}
}

func TestDiffFirstApprovalCard(t *testing.T) {
	th := Dark()
	diff := "--- a/auth.go\n+++ b/auth.go\n@@ -10,3 +10,4 @@\n ctx := req.Context()\n+token := auth.Validate(ctx)\n-token := nil\n return token"
	req := ui.Request{
		ID:        "app-1",
		Rule:      "fs.write",
		Path:      "auth.go",
		Subject:   "path",
		Reason:    "update token validation",
		Diff:      diff,
		Grantable: true,
	}
	card := NewApprovalCard(req, nil)
	lines, _, _ := card.content(th, 80)
	foundAdd := false
	foundDel := false
	foundHunk := false
	for _, l := range lines {
		s := l.Text()
		if strings.Contains(s, "+token := auth.Validate") {
			foundAdd = true
		}
		if strings.Contains(s, "-token := nil") {
			foundDel = true
		}
		if strings.Contains(s, "@@ -10,3 +10,4 @@") {
			foundHunk = true
		}
	}
	if !foundAdd || !foundDel || !foundHunk {
		t.Fatalf("expected syntax diff lines in approval card, found add=%v del=%v hunk=%v", foundAdd, foundDel, foundHunk)
	}
}

func TestAutosuggestionFishStyle(t *testing.T) {
	p := NewPrompt()
	p.history = []string{"git status", "git diff HEAD~1", "cargo test"}
	p.histIdx = len(p.history)

	p.SetValue("git d")
	sug := p.Autosuggestion()
	if sug != "iff HEAD~1" {
		t.Fatalf("expected autosuggestion 'iff HEAD~1', got %q", sug)
	}

	p.MoveEnd()
	if p.Value() != "git diff HEAD~1" {
		t.Fatalf("expected MoveEnd to accept autosuggestion, got %q", p.Value())
	}
}

func TestKillRingAndUndo(t *testing.T) {
	p := NewPrompt()
	p.SetValue("hello brave new world")
	p.cursor = 15 // after "hello brave new"

	p.DeleteWord()
	if len(p.KillRing()) == 0 || p.KillRing()[len(p.KillRing())-1] != "new" {
		t.Fatalf("expected 'new' in killRing, got %v", p.KillRing())
	}

	// Undo restores deleted word
	p.Undo()
	if p.Value() != "hello brave new world" {
		t.Fatalf("expected Undo to restore text, got %q", p.Value())
	}

	// Move to end and yank
	p.MoveEnd()
	p.DeleteWord() // deletes "world"
	p.Yank()       // yanks "world" back
	if p.Value() != "hello brave new world" {
		t.Fatalf("expected Yank to restore deleted word, got %q", p.Value())
	}
}

func TestApprovalQueuePositionAndWaitTime(t *testing.T) {
	th := Dark()
	req := ui.Request{
		ID:        "app-1",
		Rule:      "fs.write",
		Grantable: true,
	}
	card := NewApprovalCard(req, nil)
	card.Index = 2
	card.Total = 3
	card.arrived = time.Now().Add(-75 * time.Second)

	b := render.NewBuffer(80, 15)
	card.Draw(b, render.Rect{X: 0, Y: 0, W: 80, H: 15}, th, 0, false)
	out := bufferString(b)
	if !strings.Contains(out, "approval 2 of 3") {
		t.Fatalf("expected queue position 'approval 2 of 3' in title, got %q", out)
	}
	if !strings.Contains(out, "blocked 1m") {
		t.Fatalf("expected wait time in subtitle, got %q", out)
	}
}

func TestApprovalDirectKeyMnemonic(t *testing.T) {
	app, _ := newTestApp()
	v := app.Current()
	reply := make(chan ui.Decision, 1)
	req := ui.Request{
		ID:        "app-1",
		Rule:      "fs.write",
		Grantable: true,
	}
	v.PushApproval(NewApprovalCard(req, reply))

	// 'd' in choose stage denies directly
	app.Dispatch(ActionKey{Binding: "d"})
	select {
	case dec := <-reply:
		if dec.Allow {
			t.Fatalf("expected deny from 'd' key, got allow")
		}
	default:
		t.Fatalf("expected decision on reply channel")
	}
}

func TestGitContextChip(t *testing.T) {
	th := Dark()
	st := Status{
		Posture:   "strict",
		GitBranch: "feature-branch",
		GitAhead:  2,
		GitDirty:  3,
	}
	b := render.NewBuffer(120, 1)
	DrawStatusBar(b, render.Rect{X: 0, Y: 0, W: 120, H: 1}, th, st, 0, nil, false)
	out := bufferString(b)
	if !strings.Contains(out, "feature-branch↑2 ●3") {
		t.Fatalf("expected git context chip in status bar, got %q", out)
	}
}

func TestLargePasteCollapsing(t *testing.T) {
	app, _ := newTestApp()
	v := app.Current()
	v.Focus = CtxPrompt

	largeText := strings.Repeat("function doSomethingImportant() {\n  return 42;\n}\n", 10)
	app.Dispatch(ActionPaste{Text: largeText})

	raw := v.Prompt.RawValue()
	if !strings.Contains(raw, "[pasted") {
		t.Fatalf("expected collapsed paste chip in prompt buffer, got: %q", raw)
	}

	// Value() on submit expands to the full text
	submitted := v.Prompt.Value()
	if submitted != largeText {
		t.Fatalf("expected Value() to expand paste chips on submission")
	}

	// Ctrl+O expands paste chip in place
	app.Dispatch(ActionKey{Binding: "ctrl+o"})
	if v.Prompt.RawValue() != largeText {
		t.Fatalf("expected Ctrl+O to expand paste chip into raw buffer")
	}
}

func TestSmartCodeCopy(t *testing.T) {
	sb := NewScrollback()
	// Entry with single code block
	codeEntry := ui.Event{
		Kind: ui.KindNotice,
		Text: "Here is the implementation:\n```go\nfunc add(a, b int) int {\n\treturn a + b\n}\n```\nHope that helps!",
	}
	sb.Append(codeEntry)
	sb.selected = 0

	copied := sb.SelectedText()
	expected := "func add(a, b int) int {\n\treturn a + b\n}"
	if copied != expected {
		t.Fatalf("expected smart code extraction, got %q", copied)
	}

	// Entry with multiple code blocks copies full text
	multiEntry := ui.Event{
		Kind: ui.KindNotice,
		Text: "First block:\n```go\nvar x = 1\n```\nSecond block:\n```go\nvar y = 2\n```",
	}
	sb.Append(multiEntry)
	sb.selected = 1
	if sb.SelectedText() != multiEntry.Text {
		t.Fatalf("expected full text for multiple code blocks, got %q", sb.SelectedText())
	}
}

func TestSessionRename(t *testing.T) {
	app, _ := newTestApp()
	v := app.Current()
	v.SetTitle("fix-auth-flow")

	if v.Title != "fix-auth-flow" {
		t.Fatalf("expected title 'fix-auth-flow', got %q", v.Title)
	}

	th := Dark()
	b := render.NewBuffer(80, 1)
	DrawSessionTabBar(b, render.Rect{X: 0, Y: 0, W: 80, H: 1}, th, []*AgentView{v}, 0, 0)
	out := bufferString(b)
	if !strings.Contains(out, "1:fix-auth-flow") {
		t.Fatalf("expected tab bar to show session title, got %q", out)
	}
}

func TestShortcutBarOverflow(t *testing.T) {
	th := Dark()
	b := render.NewBuffer(40, 1)
	DrawShortcutBar(b, render.Rect{X: 0, Y: 0, W: 40, H: 1}, th, CtxScrollback)
	out := bufferString(b)
	if !strings.Contains(out, "F1 more") {
		t.Fatalf("expected overflow 'F1 more' indicator on truncated bar, got %q", out)
	}
}
