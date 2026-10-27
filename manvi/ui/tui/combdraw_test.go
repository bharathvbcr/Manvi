package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// A tool result or model reply that is one line of combining marks is not
// touched by ui.Sanitize and wraps to a single row because the marks are zero
// width. Each frame that shows it used to take seconds, because the buffer
// appended every mark to one cell with no bound.
func TestACombiningMarkRunDoesNotStallTheFrame(t *testing.T) {
	a, _ := newTestApp()
	text := ui.Sanitize("a" + strings.Repeat("́", 200_000))
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindText, Text: text}})
	buf := render.NewBuffer(100, 30)
	start := time.Now()
	a.Draw(buf)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("one Draw of a transcript holding 200,000 combining marks took %s", took)
	}
}
