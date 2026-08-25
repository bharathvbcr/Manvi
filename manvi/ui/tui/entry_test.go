package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/ui"
	"manvi/ui/render"
)

func lineText(lines []render.Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text())
		b.WriteString("\n")
	}
	return b.String()
}

func TestAQualifiedPassIsNeverStyledAsACleanOne(t *testing.T) {
	// This is the rendering half of the harness's central invariant. A tool
	// call that succeeded only because a grant cleared it, or with a check that
	// could not run, must not carry the mark reserved for rules that passed.
	cases := []struct {
		name  string
		event ui.Event
		want  StatusKind
	}{
		{
			"clean success",
			ui.Event{Kind: ui.KindToolResult, Text: "wrote it"},
			StatusPass,
		},
		{
			"allowed by a grant",
			ui.Event{Kind: ui.KindToolResult, Text: "wrote it", GrantID: "GRANT-1", GrantedBy: "human"},
			StatusGranted,
		},
		{
			"demoted by the posture",
			ui.Event{Kind: ui.KindToolResult, Text: "wrote it", Rule: "scope.unplanned", Demoted: "dev posture"},
			StatusGranted,
		},
		{
			"a check could not run",
			ui.Event{Kind: ui.KindToolResult, Text: "verified", Degraded: []string{"secret_scan"}},
			StatusDegraded,
		},
		{
			"a hard block",
			ui.Event{Kind: ui.KindPolicy, Text: "refused", Severity: "hard", Rule: "secret.path"},
			StatusBlock,
		},
	}
	for _, c := range cases {
		e := &Entry{Event: c.event}
		if got := e.Status(); got != c.want {
			t.Errorf("%s: status = %v, want %v", c.name, got, c.want)
		}
		if c.want != StatusPass && e.marker(Dark().Glyphs()) == Dark().Glyphs().Pass {
			t.Errorf("%s: carries the pass marker", c.name)
		}
	}
}

func TestQualificationsAreAlwaysRendered(t *testing.T) {
	th := Dark()
	th.Unicode = true

	e := &Entry{Event: ui.Event{
		Kind: ui.KindToolResult, Text: "wrote src/helper.go",
		Rule: "scope.unplanned", GrantID: "GRANT-7", GrantedBy: "human",
		Demoted: "dev posture", Degraded: []string{"secret_scan", "diff_coverage"},
	}}
	got := lineText(e.Lines(th, 80))

	for _, want := range []string{
		"GRANT-7", "human", "not a clean pass",
		"dev posture", "would have blocked",
		"secret_scan", "diff_coverage", "checks that did not run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering omitted %q:\n%s", want, got)
		}
	}
}

func TestDegradedChecksAreNotHiddenByFolding(t *testing.T) {
	// Printing nothing here is what would make an unexamined result look
	// examined, so folding must not be able to hide it either — a folded entry
	// still carries the degraded marker in its gutter.
	th := Dark()
	e := &Entry{
		Event:    ui.Event{Kind: ui.KindToolResult, Text: strings.Repeat("output\n", 40), Degraded: []string{"secret_scan"}},
		Foldable: true,
		Folded:   true,
	}
	lines := e.Lines(th, 80)
	if len(lines) != 1 {
		t.Fatalf("a folded entry rendered %d lines", len(lines))
	}
	if e.Status() != StatusDegraded {
		t.Fatal("a folded entry lost its degraded status")
	}
	if !strings.HasPrefix(lines[0].Text(), th.Glyphs().Degraded) {
		t.Fatalf("folded line %q does not lead with the degraded marker", lines[0].Text())
	}
}

func TestDevPostureIsRestatedOnEverySessionStart(t *testing.T) {
	th := Dark()
	e := &Entry{Event: ui.Event{Kind: ui.KindSessionStart, Posture: "dev", Model: "some-model"}}
	got := lineText(e.Lines(th, 90))
	if !strings.Contains(got, "still block") {
		t.Fatalf("the dev-posture caveat is missing:\n%s", got)
	}

	e = &Entry{Event: ui.Event{Kind: ui.KindSessionStart, Posture: "strict", Model: "some-model"}}
	if got := lineText(e.Lines(th, 90)); strings.Contains(got, "still block") {
		t.Fatalf("a strict session printed the dev caveat:\n%s", got)
	}
}

func TestSessionStartAdvertisesRepositoryQuickActions(t *testing.T) {
	e := &Entry{Event: ui.Event{Kind: ui.KindSessionStart, Posture: "strict", Model: "some-model"}}
	got := lineText(e.Lines(Dark(), 120))
	for _, want := range []string{"Quick:", "/pull", "/push", "/issues", "Ctrl+P", "palette"} {
		if !strings.Contains(got, want) {
			t.Errorf("session guide omitted %q:\n%s", want, got)
		}
	}
}

func TestRenderCacheIsKeyedOnEverythingThatChangesOutput(t *testing.T) {
	// Keying on width alone leaves stale lines on screen after a fold or a
	// theme change, and the bug reads as a rendering glitch rather than a cache
	// miss.
	e := &Entry{
		Event:    ui.Event{Kind: ui.KindToolResult, Text: "one\ntwo\nthree"},
		Foldable: true,
	}
	expanded := len(e.Lines(Dark(), 60))
	e.Folded = true
	folded := len(e.Lines(Dark(), 60))
	if folded >= expanded {
		t.Fatalf("folding returned %d lines against %d expanded — the cache was stale", folded, expanded)
	}
	e.Folded = false
	if got := len(e.Lines(Light(), 60)); got != expanded {
		t.Fatalf("a theme switch returned %d lines, want %d", got, expanded)
	}
}

func TestToolArgumentsAreFormattedNotEchoed(t *testing.T) {
	// Arguments are model-authored. Echoing the raw string would put whatever
	// the model wrote — newlines and escapes included — into the frame.
	args, _ := json.Marshal(map[string]any{
		"path":    "src/x.go",
		"content": "line one\nline two",
	})
	got := toolDetail(ui.Event{Kind: ui.KindToolStart, Tool: "write", Arguments: args})
	if strings.Contains(got, "\n") {
		t.Fatalf("a newline survived into the tool banner: %q", got)
	}
	if !strings.Contains(got, "path=src/x.go") {
		t.Fatalf("detail = %q", got)
	}
	// Ordering must be stable: map iteration is random, and a banner that
	// reorders itself between frames reads as the call changing.
	for i := 0; i < 20; i++ {
		if toolDetail(ui.Event{Arguments: args}) != got {
			t.Fatal("tool detail is not stable across renders")
		}
	}
}

func TestUnparseableArgumentsRenderNothingRatherThanGarbage(t *testing.T) {
	if got := toolDetail(ui.Event{Arguments: json.RawMessage(`{not json`)}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestScrollbackMergesStreamedText(t *testing.T) {
	// A turn produces hundreds of text deltas. One entry each would make the
	// transcript unnavigable and folding meaningless.
	s := NewScrollback()
	for _, chunk := range []string{"Hello", ", ", "world"} {
		s.Append(ui.Event{Kind: ui.KindText, Text: chunk})
	}
	if s.Len() != 1 {
		t.Fatalf("%d entries, want 1", s.Len())
	}
	if got := s.Entries()[0].text(); got != "Hello, world" {
		t.Fatalf("merged text = %q", got)
	}

	// A different kind breaks the run.
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read"})
	s.Append(ui.Event{Kind: ui.KindText, Text: "after"})
	if s.Len() != 3 {
		t.Fatalf("%d entries, want 3", s.Len())
	}
}

func TestRetiredEntriesAreReportedNotSilentlyDropped(t *testing.T) {
	// A transcript that silently loses its beginning is a transcript that
	// cannot be used as evidence.
	s := NewScrollback()
	s.MaxEntries = 10
	for i := 0; i < 25; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: "n" + itoa(i)})
	}
	if s.Len() != 10 {
		t.Fatalf("%d entries retained, want 10", s.Len())
	}
	if s.Dropped() != 15 {
		t.Fatalf("dropped = %d, want 15", s.Dropped())
	}

	b := render.NewBuffer(60, 14)
	s.ScrollToTop()
	s.Draw(b, render.Rect{X: 0, Y: 0, W: 60, H: 14}, Dark(), false)
	frame := ""
	for y := 0; y < 14; y++ {
		for x := 0; x < 60; x++ {
			frame += string(b.Cell(x, y).R)
		}
		frame += "\n"
	}
	if !strings.Contains(frame, "15 earlier entries") {
		t.Fatalf("the retention notice is missing from the frame:\n%s", frame)
	}
}

func TestScrollingAwayFromTheBottomStopsFollowing(t *testing.T) {
	// An operator who scrolls up to read something must not be dragged away
	// from it by the next event.
	s := NewScrollback()
	for i := 0; i < 100; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: "entry " + itoa(i)})
	}
	b := render.NewBuffer(60, 10)
	rect := render.Rect{X: 0, Y: 0, W: 60, H: 10}
	s.Draw(b, rect, Dark(), false)
	if !s.Following() {
		t.Fatal("a fresh transcript should follow the newest row")
	}

	s.ScrollBy(-20)
	if s.Following() {
		t.Fatal("scrolling up did not stop following")
	}
	before := s.scroll
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "new"})
	s.Draw(b, rect, Dark(), false)
	if s.scroll != before {
		t.Fatalf("a new event moved a scrolled-back viewport from %d to %d", before, s.scroll)
	}

	s.ScrollToBottom()
	if !s.Following() {
		t.Fatal("returning to the bottom did not resume following")
	}
}

func TestWeakenedSettingsAreNotReportedAsUnrunChecks(t *testing.T) {
	// A relaxed setting and a check that could not run are different facts, and
	// an operator's response to each is different: one is a decision to revisit,
	// the other is infrastructure to fix. Reporting the first as the second
	// sends them to the wrong place.
	th := Dark()
	e := &Entry{Event: ui.Event{
		Kind:     ui.KindNotice,
		Text:     "results produced under these settings are not strict",
		Weakened: []string{"harness.posture = dev (default)"},
	}}
	got := lineText(e.Lines(th, 90))
	if strings.Contains(got, "did not run") {
		t.Fatalf("a weakened setting was reported as an unrun check:\n%s", got)
	}
	if !strings.Contains(got, "safety settings off default") {
		t.Fatalf("the weakened setting was not named:\n%s", got)
	}
	if !e.Event.Qualified() {
		t.Fatal("a weakened setting did not mark the event as qualified")
	}
}

func TestShortTranscriptSitsAgainstTheComposer(t *testing.T) {
	// A transcript grows downward toward the thing you type into. Top-aligning
	// a short one strands the newest line furthest from where the eye already
	// is, under a band of empty rows.
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "only line"})

	b := render.NewBuffer(60, 12)
	s.Draw(b, render.Rect{X: 0, Y: 0, W: 60, H: 12}, Dark(), false)

	rowOf := func(y int) string {
		var sb strings.Builder
		for x := 0; x < 60; x++ {
			sb.WriteRune(b.Cell(x, y).R)
		}
		return strings.TrimSpace(sb.String())
	}
	if got := rowOf(11); !strings.Contains(got, "only line") {
		t.Fatalf("bottom row = %q, want the single entry", got)
	}
	if got := rowOf(0); got != "" {
		t.Fatalf("top row = %q, want empty", got)
	}
}

func TestClickStillLandsCorrectlyWhenContentIsBottomAligned(t *testing.T) {
	// Bottom-alignment shifts every row, so the row-to-entry mapping has to
	// shift with it or every click in a short transcript is off by the gap.
	s := NewScrollback()
	for i := 0; i < 3; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: "entry " + itoa(i)})
	}
	b := render.NewBuffer(60, 12)
	s.Draw(b, render.Rect{X: 0, Y: 0, W: 60, H: 12}, Dark(), false)

	if !s.SelectAt(11) {
		t.Fatal("clicking the bottom row selected nothing")
	}
	if got := s.Selected(); got == nil || !strings.Contains(got.text(), "entry 2") {
		t.Fatalf("bottom row selected %v, want the newest entry", got)
	}
}

// The session's first transcript line carries the mark at its one-line rung,
// and still carries the posture and model that line exists to report.
func TestSessionStartLineCarriesTheMark(t *testing.T) {
	th := Dark()
	th.Unicode = true
	e := &Entry{Event: ui.Event{Kind: ui.KindSessionStart, Posture: "strict", Model: "some-model"}}
	got := lineText(e.Lines(th, 80)[:1])
	for _, want := range []string{"█", "manvi", "strict", "some-model"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the session line is missing %q: %q", want, got)
		}
	}

	// A pane too narrow for the tile keeps the name: the branding degrades, the
	// report does not.
	plain := NoColorTheme()
	got = lineText(e.Lines(plain, 40)[:1])
	if strings.Contains(got, "█") {
		t.Fatalf("the plain theme drew a tile: %q", got)
	}
	if !strings.Contains(got, "manvi") || !strings.Contains(got, "some-model") {
		t.Fatalf("the plain session line lost content: %q", got)
	}
}
