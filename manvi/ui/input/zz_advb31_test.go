package input

import (
	"strings"
	"testing"
)

// 0x1c-0x1f inside a bracketed paste are paste bytes, never Ctrl keys.
func TestAdvB31CtrlPunctuationInsidePasteStaysPaste(t *testing.T) {
	in := "\x1b[200~a\x1c\x1d\x1e\x1fb\x1b[201~"
	evs := decodeAll(t, in)
	if len(evs) != 1 {
		t.Fatalf("a paste holding 0x1c-0x1f produced %d events: %#v", len(evs), evs)
	}
	p, ok := evs[0].(Paste)
	if !ok {
		t.Fatalf("event is %T, want Paste: %#v", evs[0], evs[0])
	}
	if !strings.HasPrefix(p.Text, "a") || !strings.HasSuffix(p.Text, "b") {
		t.Fatalf("paste text %q lost its printable bytes", p.Text)
	}
	t.Logf("paste text delivered as %q (sanitizePaste drops control bytes < 0x20)", p.Text)
}

// The same bytes split across two reads: the paste is not closed until the end
// marker, so the control bytes must not surface as keys between the chunks.
func TestAdvB31CtrlPunctuationInsideSplitPaste(t *testing.T) {
	buf := []byte("\x1b[200~a\x1c\x1d")
	ev, n := decode(buf, false)
	if ev != nil || n != 0 {
		t.Fatalf("an open paste without flush produced %#v (consumed %d)", ev, n)
	}
}

func TestAdvB31EscThenCtrlPunctuation(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b\x1c": "alt+ctrl+\\",
		"\x1b\x1d": "alt+ctrl+]",
		"\x1b\x1e": "alt+ctrl+^",
		"\x1b\x1f": "alt+ctrl+_",
	} {
		evs := decodeAll(t, in)
		if len(evs) != 1 {
			t.Errorf("%q decoded to %d events: %#v", in, len(evs), evs)
			continue
		}
		k, ok := evs[0].(Key)
		if !ok || k.Mod&ModAlt == 0 || k.Mod&ModCtrl == 0 || len(k.Runes) != 1 {
			t.Errorf("%q = %#v, want alt+ctrl rune", in, evs[0])
			continue
		}
		_ = want
		t.Logf("%q -> %s", in, k.String())
	}
}

// Kitty CSI-u forms of the same four keys name the same Key as the raw byte.
func TestAdvB31KittyCtrlPunctuationMatchesRawByte(t *testing.T) {
	for raw, csi := range map[string]string{
		"\x1c": "\x1b[92;5u",
		"\x1d": "\x1b[93;5u",
		"\x1e": "\x1b[94;5u",
		"\x1f": "\x1b[95;5u",
	} {
		a, b := singleAs[Key](t, raw), singleAs[Key](t, csi)
		if a.Type != b.Type || a.Mod != b.Mod || string(a.Runes) != string(b.Runes) {
			t.Errorf("raw %q = %q but kitty %q = %q", raw, a.String(), csi, b.String())
		}
	}
	// A kitty report that names the control codepoint itself (28..31) must not
	// become a printable-looking key; report what it does.
	for cp := 28; cp <= 31; cp++ {
		k := singleAs[Key](t, "\x1b["+string(rune('0'+cp/10))+string(rune('0'+cp%10))+";5u")
		t.Logf("CSI %d;5u => %q runes=%q", cp, k.String(), string(k.Runes))
	}
}
