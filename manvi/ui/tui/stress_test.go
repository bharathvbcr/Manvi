package tui

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"manvi/ui"
	"manvi/ui/render"
)

// fuzzActions is the alphabet the stress below draws from: every key the UI
// binds, printable runes, pointer events, and the harness events that arrive
// from outside the loop.
func fuzzActions(rng *rand.Rand) Action {
	switch rng.Intn(10) {
	case 0, 1, 2:
		keys := []string{
			"enter", "tab", "esc", "up", "down", "left", "right", "backspace",
			"ctrl+p", "ctrl+g", "ctrl+s", "ctrl+t", "ctrl+n", "ctrl+w", "ctrl+c",
			"ctrl+q", "ctrl+d", "space", "home", "end", "pgup", "pgdn", "e", "y", "j", "k",
		}
		return ActionKey{Binding: keys[rng.Intn(len(keys))]}
	case 3, 4:
		runes := []rune("/abcdefghijklmnopqrstuvwxyz0123456789 .-_@")
		return ActionRune{Runes: []rune{runes[rng.Intn(len(runes))]}}
	case 5:
		return ActionClick{X: rng.Intn(100), Y: rng.Intn(30), Button: 1}
	case 6:
		return ActionScroll{X: rng.Intn(100), Y: rng.Intn(30), Delta: rng.Intn(7) - 3}
	case 7:
		return ActionMotion{X: rng.Intn(100), Y: rng.Intn(30), Button: rng.Intn(2)}
	case 8:
		kinds := []ui.Kind{ui.KindText, ui.KindToolStart, ui.KindToolResult, ui.KindNotice, ui.KindError, ui.KindPolicy}
		return ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: kinds[rng.Intn(len(kinds))], At: time.Now().UTC(),
			Text: strings.Repeat("x", rng.Intn(40)),
		}}
	default:
		return ActionTick{}
	}
}

// TestRandomInputNeverPanicsAndNeverStartsATurnDuringOne.
//
// The seeds are fixed, so a failure is reproducible from the log line alone.
// Two things are asserted across a long random stream. The first is that
// nothing panics: the dispatcher, the overlays, the composer and the painter
// are all driven from the same keys an operator has, and a panic in raw mode on
// an alternate screen leaves a terminal nobody can type into.
//
// The second is the guard this change added. A slash command started while a
// turn is running is dropped by the runner, and used to be dropped in silence.
// No sequence of keys, clicks and events may produce one.
func TestRandomInputNeverPanicsAndNeverStartsATurnDuringOne(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		a, _ := newTestApp()
		busy := false

		for step := 0; step < 4000; step++ {
			act := fuzzActions(rng)
			effects := func() []Effect {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("seed %d step %d: %#v panicked: %v", seed, step, act, r)
					}
				}()
				return a.Dispatch(act)
			}()

			for _, e := range effects {
				switch e.(type) {
				case EffectCommand, EffectSubmit:
					if busy {
						t.Fatalf("seed %d step %d: %#v started work while a turn was running", seed, step, e)
					}
					busy = true
				}
			}
			// The runner's own bookkeeping, mirrored: a turn ends, and the App
			// is told, which is what clears Status.Busy.
			if busy && rng.Intn(6) == 0 {
				a.Dispatch(ActionTurnEnded{SessionID: a.Current().ID})
				busy = false
			}
			if busy {
				a.Dispatch(ActionTurnStarted{SessionID: a.Current().ID})
			}
			if a.Quitting() {
				break
			}
			if step%97 == 0 {
				b := render.NewBuffer(100, 30)
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("seed %d step %d: Draw panicked: %v", seed, step, r)
						}
					}()
					a.Draw(b)
				}()
			}
		}
	}
}

// TestChoosingInTheSettingsPickerNeverEmitsAnEffect.
//
// The picker exists because the flag report was not navigable. It must stay a
// picker: choosing a row writes a command into the composer and nothing else,
// so every change still goes through /flags set — the one place that validates
// the value, refuses a startup flag, reports which direction a safety flag
// moved, and writes the whole thing into the transcript.
//
// This drives it with random keys and asserts that no accept, from any row, in
// any filter state, ever asks the harness to do anything. What the operator
// then sends from the composer is the operator's, and the fuzz below checks
// only that the picker did not compose it for them.
func TestChoosingInTheSettingsPickerNeverEmitsAnEffect(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		a, _ := newTestApp()
		a.openSettings()

		for step := 0; step < 1500; step++ {
			inPicker := a.overlay != nil && a.overlay.Kind == OverlaySettings
			before := a.Current().Prompt.Value()

			var effects []Effect
			if rng.Intn(4) == 0 {
				runes := []rune("abcdefghijklmnopqrstuvwxyz.")
				effects = a.Dispatch(ActionRune{Runes: []rune{runes[rng.Intn(len(runes))]}})
			} else {
				keys := []string{"up", "down", "tab", "enter", "esc", "backspace", "pgup", "pgdn"}
				effects = a.Dispatch(ActionKey{Binding: keys[rng.Intn(len(keys))]})
			}

			if inPicker {
				if len(effects) != 0 {
					t.Fatalf("seed %d step %d: acting in the settings picker produced %#v", seed, step, effects)
				}
				now := a.Current().Prompt.Value()
				// Either the draft is untouched, or the composer holds a set
				// command with no value filled in. The picker never chooses a
				// value; that is what keeps it from being a second way to move
				// a setting.
				if now != before && !strings.HasPrefix(now, "/flags set ") {
					t.Fatalf("seed %d step %d: the picker wrote %q into the composer", seed, step, now)
				}
				if strings.HasPrefix(now, "/flags set ") && len(strings.Fields(now)) != 3 {
					t.Fatalf("seed %d step %d: the picker composed a value: %q", seed, step, now)
				}
			}

			if a.overlay == nil && rng.Intn(3) == 0 {
				a.Current().Prompt.Clear()
				a.openSettings()
			}
		}
	}
}

// TestOverlayInvariantsHoldUnderRandomNavigation. The selection index is used
// to index the filtered slice on every accept, so an off-by-one here is a panic
// in front of an operator rather than a wrong row.
func TestOverlayInvariantsHoldUnderRandomNavigation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	specs := make([]SettingSpec, 0, 60)
	for i := 0; i < 60; i++ {
		specs = append(specs, SettingSpec{
			Key: "a.b." + strings.Repeat("k", i%7+1) + itoa(i), Value: "v", Origin: "default", Mutable: "human",
		})
	}
	o := SettingsOverlay(specs)

	for step := 0; step < 20000; step++ {
		switch rng.Intn(5) {
		case 0:
			o.Move(rng.Intn(9) - 4)
		case 1:
			o.MoveTo(rng.Intn(80) - 10)
		case 2:
			o.SetFilter(strings.Repeat("k", rng.Intn(9)))
		case 3:
			o.Query().SetValue(string(rune('a' + rng.Intn(26))))
			o.Refilter()
		default:
			o.HoverAt(rng.Intn(50), rng.Intn(50))
		}
		if o.Empty() {
			if _, ok := o.Selected(); ok {
				t.Fatalf("step %d: an empty overlay reported a selection", step)
			}
			continue
		}
		if got := o.Sel(); got < 0 || got >= len(o.filtered) {
			t.Fatalf("step %d: selection %d is outside the %d filtered rows", step, got, len(o.filtered))
		}
		if _, ok := o.Selected(); !ok {
			t.Fatalf("step %d: a non-empty overlay reported no selection", step)
		}
	}
}
