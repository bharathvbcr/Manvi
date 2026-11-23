package tui

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

func buildCapsuleTestScrollback() *Scrollback {
	s := NewScrollback()
	s.StackWork = true
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "fix the bug"})                                          // 0
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "needle-in-thought: checking parser"})                   // 1 (capsule start)
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"a.go"}`)})      // 2 (absorbed)
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "read_file", Text: "needle-in-read\nline2\n"})          // 3 (hidden in collapsed capsule)
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec_command", Arguments: []byte(`{"cmd":"go test"}`)}) // 4 (absorbed)
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "exec_command", IsError: true, Text: "FAIL pkg/x"})     // 5 (pinned!)
	s.Append(ui.Event{Kind: ui.KindPolicy, Rule: "path.protected", Severity: "hard", Text: "blocked .env"})  // 6 (pinned!)
	s.Append(ui.Event{Kind: ui.KindText, Text: "final answer"})                                              // 7
	return s
}

func TestSelectDeltaCollapsedCapsuleStopsAtHeaderAndPinned(t *testing.T) {
	th := Dark()
	s := buildCapsuleTestScrollback()
	buf := render.NewBuffer(90, 20)
	s.Draw(buf, render.Rect{X: 0, Y: 0, W: 90, H: 20}, th, true)

	// Walk upward from bottom with k (SelectDelta(-1)):
	// Expect stops: 7 (reply) -> 6 (pinned policy) -> 5 (pinned error) -> 1 (capsule header) -> 0 (turn start).
	wantUp := []int{7, 6, 5, 1, 0}
	for step, want := range wantUp {
		s.SelectDelta(-1, th)
		if s.selected != want {
			t.Fatalf("up step %d: selected=%d, want %d", step, s.selected, want)
		}
		if s.expandedCapsules[1] {
			t.Fatalf("up step %d: SelectDelta must not auto-expand collapsed capsule", step)
		}
	}

	// Walk downward with j (SelectDelta(+1)):
	// Expect stops: 1 (capsule header) -> 5 (pinned error) -> 6 (pinned policy) -> 7 (reply).
	wantDown := []int{1, 5, 6, 7}
	for step, want := range wantDown {
		s.SelectDelta(1, th)
		if s.selected != want {
			t.Fatalf("down step %d: selected=%d, want %d", step, s.selected, want)
		}
		if s.expandedCapsules[1] {
			t.Fatalf("down step %d: SelectDelta must not auto-expand collapsed capsule", step)
		}
	}

	// Pressing Enter (ToggleFold) on the capsule header expands it so interior entry 3 becomes reachable.
	s.selected = 1
	if !s.ToggleFold() {
		t.Fatal("ToggleFold on capsule header returned false")
	}
	if !s.expandedCapsules[1] {
		t.Fatal("ToggleFold on capsule header did not expand capsule")
	}
	s.SelectDelta(1, th)
	if s.selected != 3 {
		t.Fatalf("after expanding capsule, SelectDelta(+1) from header landed on %d, want 3", s.selected)
	}
}

func TestSearchExpandsCollapsedCapsuleAndRevealsMatch(t *testing.T) {
	th := Dark()
	s := buildCapsuleTestScrollback()
	buf := render.NewBuffer(90, 20)
	s.Draw(buf, render.Rect{X: 0, Y: 0, W: 90, H: 20}, th, true)

	if s.expandedCapsules[1] {
		t.Fatal("capsule should start collapsed")
	}
	if !s.SetSearch("needle-in", th) {
		t.Fatal("SetSearch found no matches for needle-in")
	}
	if !s.expandedCapsules[1] {
		t.Fatal("SetSearch must expand enclosing collapsed capsule")
	}
	if s.selected != 1 {
		t.Fatalf("first match selected=%d, want 1", s.selected)
	}

	// Collapse again and verify NextMatch expands it and unfolds the matched ToolResult (entry 3).
	if !s.ToggleCapsule(1) {
		t.Fatal("ToggleCapsule failed to collapse")
	}
	if s.expandedCapsules[1] {
		t.Fatal("capsule should be collapsed before NextMatch")
	}
	if !s.NextMatch(th) {
		t.Fatal("NextMatch returned false")
	}
	if !s.expandedCapsules[1] {
		t.Fatal("NextMatch must expand enclosing collapsed capsule")
	}
	if s.selected != 3 {
		t.Fatalf("NextMatch selected=%d, want 3", s.selected)
	}
	if s.entries[3].Folded {
		t.Fatal("NextMatch must unfold matched entry")
	}
	// Verify matched entry actually has a rendered row now.
	foundRow := false
	for _, r := range s.rows(th, 90) {
		if r.entry == 3 && !r.gap {
			foundRow = true
			break
		}
	}
	if !foundRow {
		t.Fatal("matched entry 3 has no visible row after NextMatch")
	}

	// Collapse again and verify PrevMatch expands it too.
	if !s.ToggleCapsule(1) {
		t.Fatal("ToggleCapsule failed to collapse")
	}
	if !s.PrevMatch(th) {
		t.Fatal("PrevMatch returned false")
	}
	if !s.expandedCapsules[1] {
		t.Fatal("PrevMatch must expand enclosing collapsed capsule")
	}
	if s.selected != 1 {
		t.Fatalf("PrevMatch selected=%d, want 1", s.selected)
	}
}

func TestPinnedRowsSurviveSetFoldAllTrue(t *testing.T) {
	th := Dark()
	s := buildCapsuleTestScrollback()
	// Add a granted and a degraded entry inside the same work run before the final reply.
	s.entries = s.entries[:7]
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "patch_file", Text: "patched", GrantID: "G-42", GrantedBy: "human"})
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "exec_command", Text: "ran", Weakened: []string{"posture=relaxed"}})
	s.Append(ui.Event{Kind: ui.KindText, Text: "done"})

	// Expand everything, then fold everything.
	s.SetFoldAll(false)
	s.SetFoldAll(true)

	rows := s.rows(th, 100)
	var all strings.Builder
	for _, r := range rows {
		all.WriteString(r.line.Text())
		all.WriteByte('\n')
	}
	rendered := all.String()
	for _, mustKeep := range []string{"FAIL pkg/x", "blocked .env", "G-42", "posture=relaxed"} {
		if !strings.Contains(rendered, mustKeep) {
			t.Errorf("pinned safety detail %q disappeared after SetFoldAll(true):\n%s", mustKeep, rendered)
		}
	}
}

func TestDragSelectionClearsOnReflowAndPreservesAboveStreamingTail(t *testing.T) {
	th := Dark()
	s := NewScrollback()
	s.StackWork = true
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "hello operator prompt"})
	s.Append(ui.Event{Kind: ui.KindText, Text: "streaming reply line 1"})

	buf := render.NewBuffer(80, 12)
	s.Draw(buf, render.Rect{X: 0, Y: 0, W: 80, H: 12}, th, false)

	// Select text on row 0 (TurnStart), which sits above the streaming reply.
	s.sel.Begin(0, 0)
	s.sel.Update(0, 12)
	s.sel.End()
	if !s.sel.Active {
		t.Fatal("selection should be active")
	}

	// Stream more deltas into the reply at the tail: selection above must survive!
	s.Append(ui.Event{Kind: ui.KindText, Text: "\nstreaming reply line 2"})
	s.Draw(buf, render.Rect{X: 0, Y: 0, W: 80, H: 12}, th, false)
	if !s.sel.Active {
		t.Fatal("selection above streaming tail was wrongly cleared")
	}

	// Reflow via ToggleStackWork clears selection.
	s.ToggleStackWork()
	if s.sel.Active {
		t.Fatal("ToggleStackWork must clear drag selection")
	}

	// Reflow via width change clears selection.
	s.Draw(buf, render.Rect{X: 0, Y: 0, W: 80, H: 12}, th, false)
	s.sel.Begin(0, 0)
	s.sel.Update(0, 10)
	s.sel.End()
	bufNarrow := render.NewBuffer(60, 12)
	s.Draw(bufNarrow, render.Rect{X: 0, Y: 0, W: 60, H: 12}, th, false)
	if s.sel.Active {
		t.Fatal("width change in Draw must clear drag selection")
	}

	// Reflow via transition from non-scrollable (W) to scrollable (W-1) clears selection.
	s2 := NewScrollback()
	s2.Append(ui.Event{Kind: ui.KindTurnStart, Text: "row zero"})
	s2.Append(ui.Event{Kind: ui.KindText, Text: "line1\nline2"})
	bufSmall := render.NewBuffer(40, 6)
	s2.Draw(bufSmall, render.Rect{X: 0, Y: 0, W: 40, H: 6}, th, false)
	if s2.ScrollbarCol() != -1 {
		t.Fatalf("expected non-scrollable initially, barCol=%d", s2.ScrollbarCol())
	}
	s2.sel.Begin(0, 0)
	s2.sel.Update(0, 8)
	s2.sel.End()
	s2.Append(ui.Event{Kind: ui.KindText, Text: "\nline3\nline4\nline5\nline6\nline7"})
	s2.Draw(bufSmall, render.Rect{X: 0, Y: 0, W: 40, H: 6}, th, false)
	if s2.ScrollbarCol() == -1 {
		t.Fatal("expected scrollable after adding lines")
	}
	if s2.sel.Active {
		t.Fatal("W -> W-1 scrollbar transition must clear drag selection")
	}

	// Reflow via live work run collapsing into a capsule clears selection overlapping the run.
	s3 := NewScrollback()
	s3.StackWork = true
	s3.Append(ui.Event{Kind: ui.KindTurnStart, Text: "prompt"})
	s3.Append(ui.Event{Kind: ui.KindReasoning, Text: "first thought"})
	s3.Draw(buf, render.Rect{X: 0, Y: 0, W: 80, H: 12}, th, false)
	lastRow := len(s3.layout) - 1
	s3.sel.Begin(lastRow, 0)
	s3.sel.Update(lastRow, 10)
	s3.sel.End()
	// Adding a second work entry collapses entries 1..2 into a work capsule.
	s3.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file"})
	s3.Draw(buf, render.Rect{X: 0, Y: 0, W: 80, H: 12}, th, false)
	if s3.sel.Active {
		t.Fatal("live work run collapsing into capsule must clear overlapping drag selection")
	}
}

func TestNarrowWindowClearsInspectorFocusTrap(t *testing.T) {
	th := Dark()
	v := NewAgentView("s1", "test")
	v.Inspector.Open = true
	v.Focus = CtxInspector

	// Wide terminal keeps CtxInspector.
	wideBuf := render.NewBuffer(130, 30)
	v.Draw(wideBuf, render.Rect{X: 0, Y: 0, W: 130, H: 30}, th, 0, nil, false)
	if v.Focus != CtxInspector {
		t.Fatalf("wide terminal should keep CtxInspector, got %v", v.Focus)
	}

	// Narrow terminal (< 116 cols) hides drawer and must fall back Focus to CtxPrompt.
	narrowBuf := render.NewBuffer(100, 30)
	v.Draw(narrowBuf, render.Rect{X: 0, Y: 0, W: 100, H: 30}, th, 0, nil, false)
	if v.Focus != CtxPrompt {
		t.Fatalf("narrow terminal with hidden drawer trapped Focus=%v, want CtxPrompt", v.Focus)
	}
}

func TestToggleCapsuleRejectsNonCapsuleAndUpdateDragAutoscrolls(t *testing.T) {
	th := Dark()
	s := buildCapsuleTestScrollback()
	if s.ToggleCapsule(0) {
		t.Fatal("ToggleCapsule accepted TurnStart index 0")
	}
	if s.ToggleCapsule(3) {
		t.Fatal("ToggleCapsule accepted interior capsule index 3")
	}
	if s.ToggleCapsule(7) {
		t.Fatal("ToggleCapsule accepted Text index 7")
	}
	if !s.ToggleCapsule(1) {
		t.Fatal("ToggleCapsule rejected valid capsule start index 1")
	}

	// Build a scrollable transcript to test drag autoscroll past top and bottom edges.
	long := NewScrollback()
	for i := 0; i < 20; i++ {
		long.Append(ui.Event{Kind: ui.KindText, Text: "line " + itoa(i) + "\n"})
	}
	buf := render.NewBuffer(40, 5)
	long.Draw(buf, render.Rect{X: 0, Y: 2, W: 40, H: 5}, th, false)
	startScroll := long.scroll
	if startScroll == 0 {
		t.Fatal("expected scrolled transcript at bottom")
	}
	if !long.BeginDrag(4, 4) {
		t.Fatal("BeginDrag inside viewport failed")
	}
	// Drag above top edge (Y=2) -> must autoscroll up.
	long.UpdateDrag(4, 1)
	if long.scroll >= startScroll {
		t.Fatalf("UpdateDrag above top edge did not scroll up: scroll=%d, start=%d", long.scroll, startScroll)
	}
	afterUp := long.scroll
	// Drag below bottom edge (Bottom=7) -> must autoscroll down.
	long.UpdateDrag(4, 8)
	if long.scroll <= afterUp {
		t.Fatalf("UpdateDrag below bottom edge did not scroll down: scroll=%d, afterUp=%d", long.scroll, afterUp)
	}
}

func TestStackSlashCommandAndCompactAlias(t *testing.T) {
	app := NewApp(Dark(), nil)
	app.adaptiveWork = true
	v := app.AddSession("s1", "test")
	if !v.Scroll.StackWork {
		t.Fatal("expected StackWork default true with adaptiveWork enabled")
	}

	// /stack must be advertised in uiCommands, not /compact.
	hasStack, hasCompact := false, false
	for _, spec := range uiCommands {
		if spec.Name == "stack" {
			hasStack = true
		}
		if spec.Name == "compact" {
			hasCompact = true
		}
	}
	if !hasStack || hasCompact {
		t.Fatalf("uiCommands stack=%v compact=%v, want stack=true compact=false", hasStack, hasCompact)
	}

	// /stack toggles off.
	app.runCommand("stack", "")
	if v.Scroll.StackWork {
		t.Fatal("/stack did not toggle StackWork off")
	}
	// /compact hidden alias toggles back on.
	app.runCommand("compact", "")
	if !v.Scroll.StackWork {
		t.Fatal("/compact alias did not toggle StackWork on")
	}
}
