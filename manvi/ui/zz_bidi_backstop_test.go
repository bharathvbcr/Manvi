package ui

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// Sanitize and the render buffer both treat bidi controls as hostile. The
// buffer cannot hold Sanitize's visible marker, so it drops them; this pins
// that every rune Sanitize marks is also refused by SetString, so the two
// cannot drift when a rune is added to isBidiControl.
func TestEveryRuneSanitizeMarksAsBidiIsDroppedByTheBuffer(t *testing.T) {
	n := 0
	for r := rune(0x80); r < 0x10000; r++ {
		if !isBidiControl(r) {
			continue
		}
		n++
		b := render.NewBuffer(5, 1)
		b.SetString(0, 0, "a"+string(r)+"b", render.Plain)
		if c := b.Cell(0, 0); c.Comb != "" {
			t.Errorf("U+%04X was stored as a combining mark: %q", r, c.Comb)
		}
	}
	if n == 0 {
		t.Fatal("no bidi controls found")
	}
}
