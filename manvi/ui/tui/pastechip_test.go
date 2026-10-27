package tui

import (
	"strings"
	"testing"
)

func padTo(n int, fill byte) string { return strings.Repeat(string(fill), n) }

// Prompt.Value expanded collapsed paste chips with ReplaceAll over a map, in
// random order. A pasted block that itself contains another chip's text (a
// copied transcript does) was expanded differently from run to run.
func TestPasteChipExpansionIsDeterministic(t *testing.T) {
	inner := "[pasted 250 chars]"
	a := inner + " " + padTo(300-len(inner)-1, '0') // 300 bytes, quoting the other chip
	b := padTo(250, '1')                            // 250 bytes
	if len(a) != 300 || len(b) != 250 {
		t.Fatalf("setup %d %d", len(a), len(b))
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p := NewPrompt()
		p.InsertPaste(a)
		p.InsertPaste(b)
		seen[p.Value()] = true
	}
	if len(seen) != 1 {
		t.Fatalf("Value() of one buffer produced %d different strings across 200 calls", len(seen))
	}
	for v := range seen {
		// Each chip is replaced once by what it stands for; the chip text inside
		// the first paste is that paste's own text, left as it was.
		if want := a + b; v != want {
			t.Fatalf("Value() = %.60q..., want the two pastes joined", v)
		}
	}
}

// Chips were never forgotten: after Submit the old paste stayed registered, so
// a later prompt that merely mentioned the chip text was replaced with the old
// paste, and every paste stayed referenced for the life of the session.
func TestSubmitForgetsPasteChips(t *testing.T) {
	p := NewPrompt()
	secret := "TOKEN=" + padTo(300, '7')
	chip := p.InsertPaste(secret)
	if got := p.Submit(); got != secret {
		t.Fatalf("Submit did not send the paste: %.40q", got)
	}
	p.SetValue("what does " + chip + " mean in your last answer?")
	want := "what does " + chip + " mean in your last answer?"
	if got := p.Value(); got != want {
		t.Fatalf("a later prompt was rewritten with the earlier paste: %.60q...", got)
	}
	if len(p.pastes) != 0 {
		t.Fatalf("pastes map still holds %d entries after Submit", len(p.pastes))
	}
}

// Typing the chip's words into a fresh buffer after Clear, with no SetValue in
// between, is the same leak by another route.
func TestClearForgetsPasteChips(t *testing.T) {
	p := NewPrompt()
	chip := p.InsertPaste("SECRET=" + padTo(300, 's'))
	p.Clear()
	for _, r := range "explain " + chip {
		p.Insert(r)
	}
	if got, want := p.Value(), "explain "+chip; got != want {
		t.Fatalf("typed chip text was expanded after Clear: %.60q", got)
	}
}

// A chip that was deleted from the buffer stands for nothing: typing the same
// words afterwards must not bring the paste back.
func TestADeletedChipIsNotExpandedWhenTypedAgain(t *testing.T) {
	p := NewPrompt()
	chip := p.InsertPaste("SECRET=" + padTo(300, 's'))
	for range chip {
		p.Backspace()
	}
	if got := p.Value(); got != "" {
		t.Fatalf("a deleted chip left %q in the buffer", got)
	}
	// The next paste prunes what the buffer no longer shows, so the words typed
	// after it are only words.
	other := padTo(250, 'x')
	p.InsertPaste(other)
	p.InsertString(" see " + chip)
	if got, want := p.Value(), other+" see "+chip; got != want {
		t.Fatalf("a deleted chip was expanded when its words came back: %.60q", got)
	}
}

// Recalling a sent prompt, or being handed new text, replaces the buffer; the
// chips of the buffer that went are not carried into the new one.
func TestSetValueKeepsOnlyChipsTheNewTextShows(t *testing.T) {
	p := NewPrompt()
	chip := p.InsertPaste("PASTE=" + padTo(300, 'p'))
	p.SetValue("/flags set x ")
	if len(p.pastes) != 0 {
		t.Fatalf("SetValue kept %d chips the new text does not show", len(p.pastes))
	}
	p.SetValue("keep " + chip) // not registered any more: stays literal
	if got := p.Value(); got != "keep "+chip {
		t.Fatalf("an unregistered chip was expanded: %.60q", got)
	}
}

// Two pastes of one length stay two chips, in the order they sit in the buffer,
// and one is not mistaken for the other.
func TestSameLengthPastesStayDistinct(t *testing.T) {
	p := NewPrompt()
	first, second := padTo(250, 'a'), padTo(250, 'b')
	c1 := p.InsertPaste(first)
	p.InsertString(" and ")
	c2 := p.InsertPaste(second)
	if c1 == c2 {
		t.Fatalf("two pastes share the chip %q", c1)
	}
	for i := 0; i < 50; i++ {
		if got, want := p.Value(), first+" and "+second; got != want {
			t.Fatalf("round %d: Value() = %.40q...", i, got)
		}
	}
	// A third of the same length, after the second chip was deleted and its
	// number could have been reused, is still distinct from the first.
	for range c2 {
		p.Backspace()
	}
	c3 := p.InsertPaste(padTo(250, 'c'))
	if c3 == c1 {
		t.Fatalf("a new paste reused the live chip %q", c1)
	}
	if got, want := p.Value(), first+" and "+padTo(250, 'c'); got != want {
		t.Fatalf("Value() = %.40q...", got)
	}
}

// Ctrl+O still expands the chips the buffer shows, and only those.
func TestExpandPastesExpandsWhatTheBufferShows(t *testing.T) {
	p := NewPrompt()
	a, b := padTo(260, 'a'), padTo(270, 'b')
	p.InsertPaste(a)
	c2 := p.InsertPaste(b)
	for range c2 {
		p.Backspace()
	}
	if !p.ExpandPastes() {
		t.Fatal("ExpandPastes found nothing to expand")
	}
	if got := p.RawValue(); got != a {
		t.Fatalf("expanded buffer = %.40q... (len %d), want the first paste only", got, len(got))
	}
	if p.ExpandPastes() {
		t.Fatal("a second expansion found chips that were already expanded")
	}
}

// Through the app: a secret pasted into one prompt is sent with it, and a later
// prompt that only mentions the chip's words goes out as typed.
func TestALaterPromptMentioningAChipIsNotRewrittenWithTheEarlierPaste(t *testing.T) {
	a, _ := newTestApp()
	secret := "API_KEY=" + padTo(300, 'k')
	a.Dispatch(ActionPaste{Text: secret})
	effects := key(a, "enter")
	if sub := onlySubmit(t, effects); sub.Text != secret {
		t.Fatalf("the first prompt did not carry its paste: %.40q", sub.Text)
	}
	typed := "what was [pasted 308 chars #1] in my last message?"
	typeText(a, typed)
	effects = key(a, "enter")
	if got := onlySubmit(t, effects).Text; got != typed {
		t.Fatalf("the second prompt was sent as %.60q..., want what was typed", got)
	}
}

// onlySubmit returns the one EffectSubmit the effects hold.
func onlySubmit(t *testing.T, effects []Effect) EffectSubmit {
	t.Helper()
	if len(effects) != 1 {
		t.Fatalf("got %d effects, want one submit: %#v", len(effects), effects)
	}
	sub, ok := effects[0].(EffectSubmit)
	if !ok {
		t.Fatalf("got %T, want EffectSubmit", effects[0])
	}
	return sub
}

// Delete a chip, type its label by hand: the deleted paste must not come back.
func TestARetypedLabelOfADeletedChipIsPlainText(t *testing.T) {
	p := NewPrompt()
	secret := padTo(300, 'S')
	chip := p.InsertPaste(secret)
	for range []rune(chip) {
		p.Backspace()
	}
	p.InsertString("see " + chip)
	if got, want := p.Value(), "see "+chip; got != want {
		t.Fatalf("a chip deleted by backspace expanded again when its label was typed: %.60q", got)
	}
}

// A label is issued once for the life of the prompt. Paste A, delete it, paste
// B of the same length: B must not take A's label, or Undo would show A's label
// over B's text.
func TestALabelIsNeverIssuedTwice(t *testing.T) {
	p := NewPrompt()
	a, b := padTo(300, 'A'), padTo(300, 'B')
	chipA := p.InsertPaste(a)
	for range []rune(chipA) {
		p.Backspace()
	}
	chipB := p.InsertPaste(b)
	if chipA == chipB {
		t.Fatalf("a second paste reissued the label %q", chipA)
	}
	// Undo back to the buffer that shows A's label: it must not send B.
	for i := 0; i < 1000 && p.Undo(); i++ {
		if strings.Contains(p.RawValue(), chipA) {
			if got := p.Value(); strings.Contains(got, b) {
				t.Fatalf("Undo showed %q and Value sent the other paste", chipA)
			}
			return
		}
	}
	t.Fatal("Undo never reached the buffer that showed the first label")
}

// The numbering survives Clear, so a paste made after a submit cannot take the
// label of one made before it.
func TestLabelNumbersSurviveClear(t *testing.T) {
	p := NewPrompt()
	first := p.InsertPaste(padTo(300, 'A'))
	p.Clear()
	second := p.InsertPaste(padTo(300, 'B'))
	if first == second {
		t.Fatalf("a paste after Clear reissued %q", first)
	}
}

// Undo after Clear brings back the chip's words but not its paste: a dead chip
// that submits as its own label, never as another paste or as the old one.
func TestUndoAfterClearGivesADeadChipThatSubmitsItsLabel(t *testing.T) {
	p := NewPrompt()
	a := padTo(300, 'A')
	chip := p.InsertPaste(a)
	p.Clear()
	if !p.Undo() || !strings.Contains(p.RawValue(), chip) {
		t.Skip("undo did not restore the chip's words")
	}
	if got := p.Value(); got != chip {
		t.Fatalf("a dead chip submitted %.60q, want its bare label %q", got, chip)
	}
}

// A paste whose text quotes an earlier chip's label is inserted as it is.
func TestAPasteQuotingAnEarlierLabelIsNotExpandedAgain(t *testing.T) {
	p := NewPrompt()
	n := padTo(300, 'x')
	first := p.InsertPaste(n)
	second := p.InsertPaste(first + strings.Repeat("y", 300))
	_ = second
	if got := strings.Count(p.Value(), n); got != 1 {
		t.Fatalf("the first paste was expanded %d times, want once", got)
	}
}

// Up then Down returns the draft with its chip intact.
func TestHistoryRoundTripKeepsTheDraftsPaste(t *testing.T) {
	p := NewPrompt()
	p.history = []string{"earlier"}
	p.histIdx = 1
	a := padTo(300, 'A')
	p.InsertPaste(a)
	p.HistoryPrev()
	p.HistoryNext()
	if !strings.Contains(p.Value(), a) {
		t.Fatalf("history round trip lost the paste: %.60q", p.Value())
	}
}
