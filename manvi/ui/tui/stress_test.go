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

// fuzzNavActions is the pointer-and-geometry half of the alphabet: clicks and
// releases with every button the decoder can report, hovers, drags, wheel
// notches, and resizes through degenerate sizes. The original fuzz pressed
// keys; the navigation repair round added pointer behaviour, and a fuzz that
// never releases a button or resizes the frame never reaches it.
func fuzzNavActions(rng *rand.Rand) Action {
	switch rng.Intn(7) {
	case 0:
		return ActionClick{X: rng.Intn(120), Y: rng.Intn(36), Button: rng.Intn(4)}
	case 1:
		return ActionRelease{X: rng.Intn(120), Y: rng.Intn(36), Button: rng.Intn(4)}
	case 2:
		return ActionMotion{X: rng.Intn(120), Y: rng.Intn(36), Button: rng.Intn(4)}
	case 3:
		return ActionScroll{X: rng.Intn(120), Y: rng.Intn(36), Delta: rng.Intn(9) - 4}
	case 4:
		return ActionResize{W: 1 + rng.Intn(200), H: 1 + rng.Intn(60)}
	case 5:
		keys := []string{"pgup", "pgdn", "g", "G", "h", "l", "home", "end", "shift+tab", "left", "right"}
		return ActionKey{Binding: keys[rng.Intn(len(keys))]}
	default:
		return ActionKey{Binding: "ctrl+g"} // the dashboard, the surface this fuzz stresses
	}
}

// checkNavInvariants asserts what must hold after every dispatch, in every
// state the fuzz can reach. Each clause is one of the repaired seams.
func checkNavInvariants(t *testing.T, a *App, seed int64, step int) {
	t.Helper()
	if o := a.overlay; o != nil && !o.Empty() {
		if o.Sel() < 0 || o.Sel() >= len(o.filtered) {
			t.Fatalf("seed %d step %d: overlay selection %d outside %d rows", seed, step, o.Sel(), len(o.filtered))
		}
	}
	if len(a.views) > 0 {
		if sel := a.dashboard.Selected(); sel < 0 || sel >= len(a.views) {
			t.Fatalf("seed %d step %d: dashboard selection %d outside %d sessions", seed, step, sel, len(a.views))
		}
	}
	if len(a.dashboard.rows) > len(a.views) {
		t.Fatalf("seed %d step %d: the dashboard recorded %d rows for %d sessions",
			seed, step, len(a.dashboard.rows), len(a.views))
	}
}

// TestRandomPointerAndGeometryNeverPanicsAndKeepsTheInvariants drives the
// whole app — sessions coming and going, an approval pending half the time,
// the dashboard opening over everything — with the pointer alphabet above.
// Beyond not panicking it asserts the approval seam's one invariant under
// pointer input: no EffectDecide may be produced unless a card is pending,
// from any button, at any coordinate, in any window size.
func TestRandomPointerAndGeometryNeverPanicsAndKeepsTheInvariants(t *testing.T) {
	for seed := int64(100); seed <= 112; seed++ {
		rng := rand.New(rand.NewSource(seed))
		host := &stubHost{}
		a := NewApp(Dark(), host)
		a.Dispatch(ActionResize{W: 120, H: 36})
		a.AddSession("S0", "session zero")
		pending := 0

		for step := 0; step < 4000; step++ {
			var act Action
			switch rng.Intn(10) {
			case 0, 1, 2, 3:
				act = fuzzNavActions(rng)
			case 4:
				act = fuzzActions(rng)
			case 5:
				if rng.Intn(2) == 0 && len(a.views) < 6 {
					a.AddSession("SX"+itoa(step), "session "+itoa(step))
				} else if len(a.views) > 1 {
					a.RemoveSession(a.views[rng.Intn(len(a.views))].ID)
				}
				continue
			case 6:
				if v := a.Current(); v != nil && v.Approval() == nil && rng.Intn(2) == 0 {
					reply := make(chan ui.Decision, 1)
					a.Dispatch(ActionApprovalRequest{SessionID: v.ID, Request: blockedRequest(true), Reply: reply})
					pending++
				}
				continue
			default:
				act = fuzzActions(rng)
			}

			effects := func() []Effect {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("seed %d step %d: %#v panicked: %v", seed, step, act, r)
					}
				}()
				return a.Dispatch(act)
			}()

			for _, e := range effects {
				if _, ok := e.(EffectDecide); ok {
					if pending == 0 {
						t.Fatalf("seed %d step %d: %#v decided an approval nobody was asked", seed, step, act)
					}
					pending--
				}
			}
			checkNavInvariants(t, a, seed, step)

			if a.Quitting() {
				break
			}
			if step%89 == 0 {
				w, h := 1+rng.Intn(200), 1+rng.Intn(60)
				b := render.NewBuffer(w, h)
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("seed %d step %d: Draw at %dx%d panicked: %v", seed, step, w, h, r)
						}
					}()
					a.Draw(b)
				}()
				checkNavInvariants(t, a, seed, step)
			}
		}
	}
}
