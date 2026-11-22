package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAdvB26SubjectEventJSONOmitsEmptyAndRoundTripsOld(t *testing.T) {
	raw, _ := json.Marshal(Event{Kind: KindApproval, At: time.Unix(1, 0).UTC(), Path: "src/a.go"})
	if strings.Contains(string(raw), "subject") {
		t.Fatalf("an event with no subject carries the key: %s", raw)
	}
	var old Event
	line := `{"kind":"approval","at":"2026-01-01T00:00:00Z","text":"r","rule":"scope.unplanned","path":"src/a.go","grantable":true}`
	if err := json.Unmarshal([]byte(line), &old); err != nil || old.Subject != "" || old.Path != "src/a.go" {
		t.Fatalf("old line did not decode: %v %+v", err, old)
	}
	raw, _ = json.Marshal(Event{Kind: KindApproval, Path: "ls", Subject: "command"})
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil || back.Subject != "command" {
		t.Fatalf("round trip: %v %s %+v", err, raw, back)
	}
}

func TestAdvB26SubjectJSONSinkCarriesAndScrubsSubject(t *testing.T) {
	var out bytes.Buffer
	sink := NewJSONSink(&out, armedScrubber())
	sink.Emit(Event{Kind: KindApproval, Rule: "command.unlisted", Path: "curl -H " + zfixKey, Subject: "command"})
	if !strings.Contains(out.String(), `"subject":"command"`) {
		t.Fatalf("the sink dropped the subject: %s", out.String())
	}
	if strings.Contains(out.String(), zfixKey) {
		t.Fatalf("a credential in the subject text reached the wire: %s", out.String())
	}
	// A hostile Subject value must not be printable as anything but a label.
	out.Reset()
	sink.Emit(Event{Kind: KindApproval, Path: "x", Subject: zfixKey})
	if strings.Contains(out.String(), zfixKey) {
		t.Fatalf("a credential placed in Subject reached the wire: %s", out.String())
	}
}

// The renderer writes a hostile subject kind as the fallback label, never as
// the raw string.
func TestAdvB26SubjectRendererHostileKind(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, armedScrubber())
	r.Emit(Event{Kind: KindApproval, Rule: "r", Severity: "soft", Path: "p", Subject: "\x1b[31mcommand"})
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("raw ESC reached the line renderer: %q", out.String())
	}
}

func TestAdvB26SubjectDecisionRowsHostileAndBounded(t *testing.T) {
	cases := map[string]string{
		"esc": "ls \x1b[2K; rm -rf ~", "bidi": "echo \u202e hsiloof", "cr": "echo safe\rrm -rf ~",
		"zw": "echo a" + strings.Repeat("\u200b", 5000) + "; rm -rf ~", "combining": "e" + strings.Repeat("\u0301", 3000) + "; rm",
		"wide": strings.Repeat("\u4e2d", 5000), "ws": "ls" + strings.Repeat(" ", 100000) + "x",
	}
	for name, subj := range cases {
		rows := DecisionRows("command", subj, 100)
		var all strings.Builder
		for _, r := range rows {
			if w := len([]rune(r.Text)); w > 200 && name != "zw" && name != "combining" {
				t.Errorf("%s: a row holds %d runes", name, w)
			}
			all.WriteString(r.Text)
		}
		for _, bad := range []string{"\x1b", "\u202e", "\r"} {
			if strings.Contains(all.String(), bad) {
				t.Errorf("%s: DecisionRows passes raw %q through for the face to deal with", name, bad)
			}
		}
	}
}

func TestAdvB26SubjectDecisionRowsMegabyteBounded(t *testing.T) {
	rows := DecisionRows("command", strings.Repeat("a", 1<<20), 100)
	if len(rows) > 500 {
		t.Fatalf("a 1 MB subject becomes %d rows on every face", len(rows))
	}
}

// A row boundary must not hide a credential from the line renderer's scrubber.
func TestAdvB26SubjectCredentialStraddlingRowBoundary(t *testing.T) {
	for pad := 80; pad < 100; pad++ {
		var out bytes.Buffer
		r := NewRenderer(&out, armedScrubber())
		r.Emit(Event{Kind: KindApproval, Rule: "r", Severity: "soft", Subject: "command",
			Path: strings.Repeat("a", pad) + " " + zfixKey})
		if strings.Contains(strings.ReplaceAll(out.String(), "\n    "+strings.Repeat(" ", 4), ""), zfixKey) {
			t.Fatalf("pad %d: credential survived: %q", pad, out.String())
		}
	}
}
