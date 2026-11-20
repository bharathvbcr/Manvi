package hygiene

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// v1Strings decodes s every way encoding/json can: as one value, and as a stream
// of values, and returns every string (and object key) any decode produced.
func v1Strings(s string) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		}
	}
	var one any
	if json.Unmarshal([]byte(s), &one) == nil {
		walk(one)
	}
	dec := json.NewDecoder(strings.NewReader(s))
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		walk(v)
	}
	return out
}

func decodesToInvisible(s string) (rune, bool) {
	for _, str := range v1Strings(s) {
		for _, r := range str {
			if Invisible(r) {
				return r, true
			}
		}
	}
	return 0, false
}

// A lone surrogate makes the document invalid for the strict validator, and
// encoding/json decodes it to U+FFFD and goes on; concatenated values are no
// single document, and json.Decoder reads each. Both still handed a parser its
// U+202E.
func TestEscapeResultNeutralisesWhatAV1DecoderReadsEvenWhenNoValidatorAcceptsTheText(t *testing.T) {
	for _, in := range []string{
		"{\"a\":\"\\ud800\",\"b\":\"\\u202e\"}",
		"{\"a\":1}\n{\"b\":\"\\u202e\"}",
		"{\"a\":1} {\"b\":\"\\udb40\\udc01\"} junk",
		"[\"\\ud800\",\"\\b\",\"\\u200b\"]",
		"{\"a\":\"\\ud800\",\"b\":\"raw\u202ehere\"}",
	} {
		before, was := decodesToInvisible(in)
		out := EscapeResult(in)
		if r, bad := decodesToInvisible(out); bad {
			t.Errorf("%q: a v1 decoder still reads %U out of %q", in, r, out)
		}
		if !was {
			t.Logf("%q: input decoded to nothing invisible (%U)", in, before)
		}
		if again := EscapeResult(out); again != out {
			t.Errorf("%q: not idempotent: %q then %q", in, out, again)
		}
	}
}

// Prose that is not JSON keeps its plain-text escaping.
func TestEscapeResultLeavesProseWithoutJSONEscapesAlone(t *testing.T) {
	for _, in := range []string{"plain", "a literal C:\\dir\\file", "evil\u202egpj"} {
		if got, want := EscapeResult(in), Escape(in); got != want {
			t.Errorf("EscapeResult(%q) = %q, want Escape's %q", in, got, want)
		}
	}
}

// Random mutations of documents full of escapes: whatever a v1 decode path reads
// out of the escaped text, it is never an Invisible rune.
func TestEscapeResultSweepNoV1DecodePathRestoresAnInvisibleRune(t *testing.T) {
	frags := []string{
		"{", "}", "[", "]", ",", ":", "\"", "\"a\"", "\"b\"", "1", "null", " ", "\n", "\r\n", "\\", "\\\\",
		"\\u202e", "\\u200b", "\\u00ad", "\\u0085", "\\u001b", "\\u2028", "\\ud800", "\\udc00", "\\ud83d\\ude00",
		"\\udb40\\udc01", "\\b", "\\f", "\\r", "\\n", "\\\"", "\\u", "\\u20", "\u202e", "\u00ad", "\x1b", "x",
	}
	rng := rand.New(rand.NewSource(1)) // #nosec G404 -- a test input generator, not a secret
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := 1 + rng.Intn(14); n > 0; n-- {
			b.WriteString(frags[rng.Intn(len(frags))])
		}
		in := b.String()
		out := EscapeResult(in)
		if r, bad := decodesToInvisible(out); bad {
			t.Fatalf("input %q: a v1 decoder reads %U out of %q", in, r, out)
		}
	}
}
