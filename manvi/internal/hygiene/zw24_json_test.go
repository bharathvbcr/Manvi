package hygiene

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// invisibleSample is a rune from every class Invisible refuses, including those
// below U+0100 that Escape spells \xNN.
var invisibleSample = []rune{
	0x00, 0x01, 0x07, 0x08, 0x0b, 0x1b, 0x1f, 0x7f, 0x80, 0x85, 0x9b, 0x9f, 0xad,
	0x200b, 0x200d, 0x200e, 0x2028, 0x2029, 0x202a, 0x202e, 0x2060, 0x2066, 0x206f,
	0xfeff, 0xe000, 0xe0001, 0xe007f,
}

func containsInvisible(s string) (rune, bool) {
	for _, r := range s {
		if Invisible(r) {
			return r, true
		}
	}
	return 0, false
}

// A JSON document that went through the escape must still be a JSON document,
// and what a parser reads out of it must not hold the rune that was hidden. The
// plain Escape fails the first (its \xNN is not a JSON escape) and the second
// (its \uNNNN decodes back to the rune).
func TestEscapeJSONLeavesAParseableDocumentWithNothingInvisibleInIt(t *testing.T) {
	for _, r := range invisibleSample {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			doc, err := json.Marshal(map[string]string{"k" + string(r): "a" + string(r) + "b"})
			if err != nil {
				t.Fatal(err)
			}
			for name, in := range map[string]string{
				"as json.Marshal wrote it": string(doc),
				"raw inside the string":    "{\"k\":\"a" + string(r) + "b\"}",
			} {
				if name == "raw inside the string" && r < 0x20 {
					continue // not valid JSON: a raw control character
				}
				out := EscapeJSON(in)
				if !json.Valid([]byte(out)) {
					t.Fatalf("%s: EscapeJSON(%q) = %q is not valid JSON", name, in, out)
				}
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				for k, v := range back {
					if bad, found := containsInvisible(k + v); found {
						t.Errorf("%s: a parser reads %U back out of %q", name, bad, out)
					}
				}
				if again := EscapeJSON(out); again != out {
					t.Errorf("%s: not idempotent: %q then %q", name, out, again)
				}
			}
		})
	}
}

func TestEscapeJSONShowsWhatWasThere(t *testing.T) {
	var got map[string]string
	out := EscapeJSON("{\"k\":\"evil\u202egpj \u00ad\\u001b\\ud83d\\ude00 \\udb40\\udc01\"}")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	for _, want := range []string{"\\u202e", "\\u00ad", "\\u001b", "\\ue0001", "\U0001F600"} {
		if !strings.Contains(got["k"], want) {
			t.Errorf("the decoded value %q does not show %q", got["k"], want)
		}
	}
}

func TestEscapeJSONLeavesLegitimateDocumentsAlone(t *testing.T) {
	for _, in := range []string{
		"{}", "[]", "null", "42", `{"a":"plain","b":["x","\n\t\"\\"],"c":"日本語 é 😀"}`,
		"{\n  \"a\": 1\n}", `"a literal \\u202e in prose"`,
	} {
		if got := EscapeJSON(in); got != in {
			t.Errorf("EscapeJSON(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestEscapeJSONKeepsAnInvalidByteInsideAStringParseable(t *testing.T) {
	out := EscapeJSON("{\"k\":\"a\xffb\"}")
	if !json.Valid([]byte(out)) || !utf8.ValidString(out) {
		t.Fatalf("%q is not a valid JSON document", out)
	}
}

// EscapeResult chooses by what the reply is: a JSON document stays one, anything
// else is escaped as text.
func TestEscapeResultChoosesByWhatTheReplyIs(t *testing.T) {
	doc := "{\"k\":\"a\u00adb\"}"
	if out := EscapeResult(doc); !json.Valid([]byte(out)) || strings.ContainsRune(out, 0xad) {
		t.Errorf("EscapeResult(%q) = %q, want valid JSON with no soft hyphen", doc, out)
	}
	const text = "see evil\u202egpj and \x1b[2J {not json"
	if got, want := EscapeResult(text), Escape(text); got != want {
		t.Errorf("EscapeResult on plain text = %q, want Escape's %q", got, want)
	}
}

// A carriage return between tokens is valid JSON whitespace and a terminal obeys
// it by overwriting the line, so a document that carries one must not leave with
// it, and must still parse.
func TestEscapeJSONDoesNotLeaveALiveCarriageReturnBetweenTokens(t *testing.T) {
	for _, in := range []string{"{\r\"a\":\"x\"}", "{\r\n\"a\":\"x\"\r\n}", "[1,\r2]", "{\"a\":\"x\"}\r"} {
		for name, out := range map[string]string{"EscapeJSON": EscapeJSON(in), "EscapeResult": EscapeResult(in)} {
			if strings.ContainsRune(out, '\r') {
				t.Errorf("%s(%q) = %q still holds a carriage return", name, in, out)
			}
			if !json.Valid([]byte(out)) {
				t.Errorf("%s(%q) = %q is not valid JSON", name, in, out)
			}
			var before, after any
			if err := json.Unmarshal([]byte(in), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(out), &after); err != nil || fmt.Sprint(before) != fmt.Sprint(after) {
				t.Errorf("%s(%q) changed what the document says: %v then %v (%v)", name, in, before, after, err)
			}
		}
	}
}
