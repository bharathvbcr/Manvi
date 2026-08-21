package tui

import (
	"strings"
	"testing"

	"manvi/ui"
)

// TestSettingsPickerIsNavigable is the complaint this whole surface answers.
//
// `manvi flags` prints a table whose last column names who may move each
// setting. It is text in a transcript: the arrow keys move between transcript
// entries, so the highlight covers the whole block rather than a row, and the
// column said "human" to an operator with no path to reach.
func TestSettingsPickerIsNavigable(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/settings")
	key(a, "enter")

	if a.overlay == nil || a.overlay.Kind != OverlaySettings {
		t.Fatal("/settings did not open the settings picker")
	}
	if a.overlay.Empty() {
		t.Fatal("the picker opened with no rows")
	}

	// Rows, not entries: down moves the highlight within the list.
	first := a.overlay.Sel()
	key(a, "down")
	if a.overlay.Sel() == first {
		t.Fatal("down did not move between settings")
	}
	key(a, "up")
	if a.overlay.Sel() != first {
		t.Fatal("up did not move back")
	}

	// And it filters, because fifty settings is a list nobody scrolls.
	typeText(a, "grants")
	if a.overlay.Empty() {
		t.Fatal("filtering to a real key matched nothing")
	}
	for _, it := range a.overlay.filtered {
		if !strings.Contains(it.Label, "grants") && !strings.Contains(it.Detail, "grants") {
			t.Fatalf("filter kept an unrelated row: %+v", it)
		}
	}
}

// TestSettingsPickerShowsValueOriginAndSafety. The picker has to answer the
// same three questions the flag table does, or it is a prettier way of knowing
// less.
func TestSettingsPickerShowsValueOriginAndSafety(t *testing.T) {
	a, _ := newTestApp()
	a.openSettings()
	if a.overlay == nil {
		t.Fatal("no picker")
	}
	var row Item
	for _, it := range a.overlay.filtered {
		if strings.Contains(it.Label, "policy.file.mode") {
			row = it
		}
	}
	if row.Value != "policy.file.mode" {
		t.Fatalf("policy.file.mode is not in the picker: %+v", a.overlay.filtered)
	}
	if !strings.Contains(row.Detail, "advisory") || !strings.Contains(row.Detail, "override") {
		t.Errorf("row = %q, want the value and where it came from", row.Detail)
	}
	if !strings.HasPrefix(row.Label, "! ") {
		t.Errorf("row = %q, want a safety setting marked as one", row.Label)
	}
	if row.Status != StatusWarn {
		t.Errorf("a safety setting off its safest value is drawn %v, want a warning", row.Status)
	}
}

// TestChoosingASettingArmsTheCommandRatherThanSettingIt.
//
// Arming is the design. Every move then goes through /flags set, which is the
// one place that validates the value, refuses a startup flag, reports which
// direction a safety flag moved, and reloads what the change invalidated. A
// picker that flipped policy.file.mode on one keypress would be a second way to
// change a setting, and the second way is the one with no record.
func TestChoosingASettingArmsTheCommandRatherThanSettingIt(t *testing.T) {
	a, _ := newTestApp()
	a.openSettings()
	typeText(a, "policy.file.mode")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("choosing a setting changed something on its own: %#v", effects)
	}
	if got := a.Current().Prompt.Value(); got != "/flags set policy.file.mode " {
		t.Fatalf("prompt = %q, want the set command waiting for a value", got)
	}
	if a.Current().Focus != CtxPrompt {
		t.Fatal("the composer was armed but focus was left elsewhere")
	}
	notice, kind := a.Notice()
	for _, want := range []string{"advisory", "override", "enforce", "safety"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice = %q, want it to mention %q", notice, want)
		}
	}
	if kind != StatusWarn {
		t.Errorf("choosing a safety setting noticed at %v, want a warning", kind)
	}

	// And the command the operator then sends is the real one.
	typeText(a, "enforce")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("sending produced %#v", effects)
	}
	cmd, ok := effects[0].(EffectCommand)
	if !ok || cmd.Name != "flags" || cmd.Args != "set policy.file.mode enforce" {
		t.Fatalf("got %#v, want the flags set command", effects[0])
	}
}

// TestChoosingAStartupSettingSaysWhyNothingHappened. Arming a command certain
// to be refused wastes the keystroke and the operator's trust in the list.
func TestChoosingAStartupSettingSaysWhyNothingHappened(t *testing.T) {
	a, _ := newTestApp()
	a.openSettings()
	typeText(a, "hard_rules")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("choosing a startup setting produced %#v", effects)
	}
	if !a.Current().Prompt.Empty() {
		t.Fatalf("the composer was armed with a command that cannot succeed: %q", a.Current().Prompt.Value())
	}
	notice, kind := a.Notice()
	if !strings.Contains(notice, "startup-only") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v), want an explanation", notice, kind)
	}
}

// TestSettingsPickerDoesNotClobberADraft.
func TestSettingsPickerDoesNotClobberADraft(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "half a thought")
	a.openSettings()
	typeText(a, "policy.file.mode")
	key(a, "enter")
	if got := a.Current().Prompt.Value(); got != "half a thought" {
		t.Fatalf("prompt = %q, want the draft untouched", got)
	}
	if notice, kind := a.Notice(); !strings.Contains(notice, "draft") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v), want an explanation of why nothing happened", notice, kind)
	}
}

// TestUIAnsweredCommandsAreInTheMenus. /sessions and /theme were implemented,
// reachable by typing the name in full, and absent from both places anyone goes
// looking. A command in neither menu is one an operator has to already know.
func TestUIAnsweredCommandsAreInTheMenus(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	if a.overlay == nil {
		t.Fatal("the palette did not open")
	}
	listed := map[string]bool{}
	for _, it := range a.overlay.filtered {
		listed[it.Value] = true
	}
	for _, want := range []string{"settings", "sessions", "theme", "doctor", "check"} {
		if !listed[want] {
			t.Errorf("the palette does not list /%s", want)
		}
	}
	key(a, "esc")

	typeText(a, "/set")
	if a.overlay == nil || a.overlay.Kind != OverlayComplete {
		t.Fatal("the completion dropdown did not open")
	}
	found := false
	for _, it := range a.overlay.filtered {
		if it.Value == "settings" {
			found = true
		}
	}
	if !found {
		t.Error("the completion dropdown does not offer /settings")
	}
}

// TestNoDuplicateRowsInThePalette. The host declares help, clear and quit as
// commands of its own; this package answers them. Listing both copies would
// make the palette look like it had two of each.
func TestNoDuplicateRowsInThePalette(t *testing.T) {
	a, _ := newTestApp()
	seen := map[string]int{}
	for _, spec := range a.menuCommands() {
		seen[spec.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("/%s appears %d times in the menus", name, n)
		}
	}
}

// TestSettingsPickerWithNothingToShowSaysSo rather than opening an empty box.
func TestSettingsPickerWithNothingToShowSaysSo(t *testing.T) {
	host := &stubHost{settings: []SettingSpec{}}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	a.AddSession("S1", "session one")

	// An empty slice is indistinguishable from nil to the stub, so force the
	// path with a host that genuinely reports none.
	host.settings = make([]SettingSpec, 0)
	a.openSettings()
	if a.overlay != nil {
		t.Fatal("the picker opened with nothing in it")
	}
	if notice, kind := a.Notice(); !strings.Contains(notice, "no settings") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v)", notice, kind)
	}
}

// TestSettingsPickerDraws. An overlay that dispatches correctly and paints
// nothing is still an unusable list, and the placement switch in drawOverlay
// has a case per kind — a new kind that fell through to nothing would be
// invisible with every test above still green.
func TestSettingsPickerDraws(t *testing.T) {
	a, _ := newTestApp()
	a.openSettings()
	got := bufferText(drawAt(a, 100, 30))

	for _, want := range []string{"settings", "policy.file.mode", "advisory", "override"} {
		if !strings.Contains(got, want) {
			t.Errorf("the drawn picker is missing %q:\n%s", want, got)
		}
	}

	// Narrow enough that the detail cannot fit: the key survives and the frame
	// is not overrun. A row truncated to nothing is a row nobody can choose.
	narrow := drawAt(a, 34, 14)
	if !strings.Contains(bufferText(narrow), "policy.file.mode") {
		t.Errorf("a narrow picker dropped the key:\n%s", bufferText(narrow))
	}
}

// TestSettingsPickerScrollsToTheSelection. Fifty settings do not fit in a box
// two-thirds of a terminal high, and a highlight that moves off the drawn
// window is a list that stops responding to the arrow keys.
func TestSettingsPickerScrollsToTheSelection(t *testing.T) {
	specs := make([]SettingSpec, 0, 50)
	for i := 0; i < 50; i++ {
		specs = append(specs, SettingSpec{
			Key: "ns." + itoa(i), Value: "v", Origin: "default", Mutable: "human",
		})
	}
	host := &stubHost{settings: specs}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	a.AddSession("S1", "session one")
	a.openSettings()

	for i := 0; i < 45; i++ {
		key(a, "down")
	}
	got := bufferText(drawAt(a, 100, 30))
	sel, _ := a.overlay.Selected()
	if !strings.Contains(got, sel.Label) {
		t.Fatalf("the highlighted row %q is not on screen:\n%s", sel.Label, got)
	}
}

// TestTighteningASettingUpdatesThePostureButKeepsTheRecord.
//
// Two fields answer two different questions and must not be collapsed. The
// posture chip answers "what is enforcing right now", so it follows a change in
// either direction. StrictSummary answers "can this session's results be
// described as strict", and a turn that ran while a safety flag was off does
// not become strict retroactively because the flag was put back.
func TestTighteningASettingUpdatesThePostureButKeepsTheRecord(t *testing.T) {
	a, _ := newTestApp()
	v := a.Current()

	v.Apply(ui.Event{Kind: ui.KindSessionStart, Posture: "dev", Model: "m"})
	v.Apply(ui.Event{
		Kind: ui.KindNotice, Text: "harness.posture is now yolo",
		Posture: "yolo", Weakened: []string{"harness.posture=yolo (override)"},
	})
	if v.Status.Posture != "yolo" {
		t.Fatalf("posture chip = %q, want it to follow the change", v.Status.Posture)
	}
	if strict, _ := v.Status.StrictSummary(); strict {
		t.Fatal("a session running under yolo reported itself as strict")
	}

	// Put it back. The chip follows; the record does not.
	v.Apply(ui.Event{
		Kind: ui.KindNotice, Text: "harness.posture is now strict",
		Posture: "strict", Weakened: nil,
	})
	if v.Status.Posture != "strict" {
		t.Fatalf("posture chip = %q, want it to follow the tightening", v.Status.Posture)
	}
	strict, why := v.Status.StrictSummary()
	if strict {
		t.Fatal("tightening a setting erased the record of the turns that ran before it")
	}
	if !strings.Contains(why, "safety flag") {
		t.Fatalf("reason = %q, want it to name the safety flag that was moved", why)
	}
}
