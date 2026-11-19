package hygiene

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func zadvHasInvisible(s string) (rune, bool) {
	for _, r := range s {
		if Invisible(r) {
			return r, true
		}
	}
	return 0, false
}

// zadvDecodedStrings returns every string (keys and values, duplicates kept)
// that encoding/json's token stream decodes from doc.
func zadvDecodedStrings(t *testing.T, doc string) []string {
	t.Helper()
	dec := stdjson.NewDecoder(strings.NewReader(doc))
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return out
			}
			t.Fatalf("not valid JSON for encoding/json: %v\n%q", err, doc)
		}
		if s, ok := tok.(string); ok {
			out = append(out, s)
		}
	}
}

// The invariant of the whole change: whatever a third party sends, what comes out
// of EscapeResult carries no Invisible rune raw, and if it was JSON it is still
// JSON that decodes (by encoding/json and by the v2 validator) to no Invisible rune.
func TestZadvW24EscapeResultJSONRoundTripNeverReintroducesAnInvisibleRune(t *testing.T) {
	invisibles := []string{"\u202e", "\u200b", "\u00ad", "\u0085", "\u2028", "\u2029", "\x1b", "\u0000", "\x7f", "\ufeff", "\U000e0041", "\ue000"}
	rng := rand.New(rand.NewSource(24))
	for i := 0; i < 3000; i++ {
		var key, val strings.Builder
		for j := 0; j < rng.Intn(6); j++ {
			key.WriteString(invisibles[rng.Intn(len(invisibles))])
			key.WriteString("k")
		}
		for j := 0; j < rng.Intn(6); j++ {
			val.WriteString(invisibles[rng.Intn(len(invisibles))])
			val.WriteString("v")
		}
		// marshal in the three ways a producer would
		var doc string
		switch i % 3 {
		case 0:
			b, _ := stdjson.Marshal(map[string]any{key.String(): val.String(), "n": []any{val.String(), 1.5, nil}})
			doc = string(b)
		case 1:
			b, _ := stdjson.MarshalIndent(map[string]any{key.String(): val.String()}, "", "\t")
			doc = string(b)
		default:
			// raw invisibles inside the strings, no escaping at all
			doc = fmt.Sprintf("{\"%s\": \"%s\"}", strings.NewReplacer("\"", "", "\\", "").Replace(key.String()), strings.NewReplacer("\"", "", "\\", "").Replace(val.String()))
		}
		for _, c := range []string{"\u0000", "\x1b"} { // raw controls are invalid JSON; skip those docs
			if i%3 == 2 && strings.Contains(doc, c) {
				doc = ""
			}
		}
		if doc == "" {
			continue
		}
		out := EscapeResult(doc)
		if r, bad := zadvHasInvisible(out); bad {
			t.Fatalf("raw Invisible %U in output of %q: %q", r, doc, out)
		}
		for _, s := range zadvDecodedStrings(t, out) {
			if r, bad := zadvHasInvisible(s); bad {
				t.Fatalf("encoding/json reintroduced %U from %q -> %q", r, doc, out)
			}
		}
		if again := EscapeResult(out); again != out {
			t.Fatalf("not idempotent:\n%q\n%q", out, again)
		}
	}
}

// A JSON document that the v2 validator rejects (a duplicate member name, which
// encoding/json accepts) takes the plain-text path. Its escapes then stay
// escapes, so the document still decodes to the hidden rune.
func TestZadvW24ADuplicateNameDocumentStillDecodesToAnInvisibleRune(t *testing.T) {
	for _, doc := range []string{
		`{"a":"x\u202ey","a":1}`,
		`{"a":1,"a":"\u200b"}`,
		`{"a":"\u00ad","b":2,"a":3}`,
	} {
		out := EscapeResult(doc)
		var v any
		if err := stdjson.Unmarshal([]byte(out), &v); err != nil {
			t.Errorf("%s: escaped output is no longer JSON a v1 parser reads: %v\n%q", doc, err, out)
			continue
		}
		for _, s := range zadvDecodedStrings(t, out) {
			if r, bad := zadvHasInvisible(s); bad {
				t.Errorf("%s: a v1 parser decodes %U out of the escaped result %q", doc, r, out)
			}
		}
	}
}

func TestZadvW24EscapeJSONEdgeInputs(t *testing.T) {
	for name, doc := range map[string]string{
		"lone high surrogate":  `"\ud800"`,
		"lone low surrogate":   `"\udc00x"`,
		"valid pair":           `"\ud83d\ude00"`,
		"pair of invisible":    `"\udb40\udc41"`,
		"escaped backslash u":  `"\\u202e"`,
		"double escaped":       `"\\\u202e"`,
		"nested json string":   `{"inner":"{\\\"a\\\":\\\"\\u202e\\\"}"}`,
		"crlf between tokens":  "{\r\n\"a\":\r\n1}",
		"lone cr between":      "[1,\r2]",
		"escaped cr in string": `"a\rb"`,
		"raw nul escape":       `"\u0000"`,
		"empty":                "",
		"deep":                 strings.Repeat("[", 500) + "\"\\u202e\"" + strings.Repeat("]", 500),
		"big":                  `"` + strings.Repeat("a\\u202e", 200000) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			out := EscapeResult(doc)
			if r, bad := zadvHasInvisible(out); bad {
				t.Errorf("raw %U in %q", r, out)
			}
			if strings.ContainsRune(out, '\r') {
				t.Errorf("a carriage return survives: %q", out)
			}
			if bytes.Equal([]byte(out), []byte(doc)) || doc == "" {
				return
			}
			if !utf8.ValidString(out) {
				t.Errorf("output is not valid UTF-8")
			}
			if stdjson.Valid([]byte(doc)) {
				for _, s := range zadvDecodedStrings(t, out) {
					if r, bad := zadvHasInvisible(s); bad {
						t.Errorf("decodes %U from %q", r, out)
					}
				}
			}
		})
	}
}

// A result that is not JSON to begin with is plain text: Escape, with no JSON
// handling, and no panic on invalid UTF-8.
func TestZadvW24NonJSONAndInvalidUTF8GoThroughPlainEscape(t *testing.T) {
	for _, in := range []string{"plain \u202e text", "{broken \xff\xfe", "\"\xff\"", "{\"a\":\"\xc3\x28\"}", "\x80"} {
		out := EscapeResult(in)
		if r, bad := zadvHasInvisible(out); bad {
			t.Errorf("%q: raw %U in %q", in, r, out)
		}
		if out != Escape(in) {
			t.Errorf("%q: not plain Escape: %q vs %q", in, out, Escape(in))
		}
	}
}
