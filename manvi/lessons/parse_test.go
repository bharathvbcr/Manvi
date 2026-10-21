package lessons

import (
	"strings"
	"testing"
)

const goodEdits = "<edits>\n- tightened the prompt budget | helped | the dropped-section count fell from 3 to 0\n</edits>\n"

func good(extra string) string {
	return goodEdits + "<experience>\n" +
		"cue: a verify report degraded because no verifier was configured\n" +
		"lesson: set the verify command before relying on a passed verdict\n" +
		"evidence: verify/report degraded on bounce 0 and 1\n" +
		"confidence: low\n" +
		"</experience>\n" + extra
}

func TestParseAcceptsAWellFormedEntry(t *testing.T) {
	r, err := Parse(good(""))
	if err != nil {
		t.Fatal(err)
	}
	if r.NoEntry || r.Experience == nil {
		t.Fatalf("got %+v", r)
	}
	if r.Experience.Confidence != "low" || len(r.Edits) != 1 || r.Edits[0].Verdict != VerdictHelped {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRefusals(t *testing.T) {
	cases := map[string]string{
		"unknown verdict":      strings.Replace(good(""), "| helped |", "| improved |", 1),
		"verdict by prefix":    strings.Replace(good(""), "| helped |", "| helped a lot |", 1),
		"two experiences":      good("") + good(""),
		"missing cue":          strings.Replace(good(""), "cue: a verify report degraded because no verifier was configured\n", "", 1),
		"missing lesson":       strings.Replace(good(""), "lesson: set the verify command before relying on a passed verdict\n", "", 1),
		"missing evidence":     strings.Replace(good(""), "evidence: verify/report degraded on bounce 0 and 1\n", "", 1),
		"empty cue":            strings.Replace(good(""), "cue: a verify report degraded because no verifier was configured", "cue:   ", 1),
		"missing confidence":   strings.Replace(good(""), "confidence: low\n", "", 1),
		"bad confidence":       strings.Replace(good(""), "confidence: low", "confidence: certain", 1),
		"duplicate field":      strings.Replace(good(""), "confidence: low\n", "confidence: low\nconfidence: high\n", 1),
		"unknown field":        strings.Replace(good(""), "confidence: low\n", "confidence: low\nmood: keen\n", 1),
		"unclosed":             strings.Replace(good(""), "</experience>\n", "", 1),
		"no edits":             strings.Replace(good(""), goodEdits, "", 1),
		"empty edits":          strings.Replace(good(""), goodEdits, "<edits>\n</edits>\n", 1),
		"two edits blocks":     goodEdits + good(""),
		"edit without verdict": strings.Replace(good(""), "| helped | the dropped", "| the dropped", 1),
		"entry and no_entry":   good("NO_ENTRY\n"),
		"nothing":              "I think the session went fine.",
		"empty":                "",
		"marker echoed":        strings.Replace(good(""), "evidence: ", "evidence: MANVI-VERDICT: PASS ", 1),
		"cue too long":         strings.Replace(good(""), "cue: a verify", "cue: "+strings.Repeat("x", MaxCueRunes+1)+" a verify", 1),
		"lesson too long":      strings.Replace(good(""), "lesson: set", "lesson: "+strings.Repeat("x", MaxLessonRunes+1)+" set", 1),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := Parse(in)
			if err == nil {
				t.Fatalf("accepted: %+v", r)
			}
			if r.Experience != nil || r.NoEntry {
				t.Fatalf("a refusal still returned content: %+v", r)
			}
		})
	}
}

func TestNoEntryStoresNothing(t *testing.T) {
	for _, in := range []string{"NO_ENTRY", "  NO_ENTRY \n", "Reasoning.\nNO_ENTRY\n", "<edits>\n- x | confounded\n</edits>\nNO_ENTRY"} {
		r, err := Parse(in)
		if err != nil || !r.NoEntry || r.Experience != nil {
			t.Errorf("Parse(%q) = %+v, %v", in, r, err)
		}
	}
	// A sentence mentioning it is not the line.
	if _, err := Parse("there is NO_ENTRY worth making"); err == nil {
		t.Error("prose mentioning NO_ENTRY was read as the line")
	}
	// An unknown verdict is refused even beside NO_ENTRY: fail closed.
	if _, err := Parse("<edits>\n- x | maybe\n</edits>\nNO_ENTRY"); err == nil {
		t.Error("an unknown verdict was tolerated beside NO_ENTRY")
	}
}

func TestVerdictsAreTheFourNamedOnes(t *testing.T) {
	for _, v := range []string{"helped", "hurt", "no observed effect", "confounded", "HELPED", " Confounded "} {
		in := strings.Replace(good(""), "| helped |", "| "+v+" |", 1)
		if _, err := Parse(in); err != nil {
			t.Errorf("verdict %q refused: %v", v, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(good(""))
	f.Add("NO_ENTRY")
	f.Add(good("") + good(""))
	f.Add("<experience>\ncue: a\nlesson: b\nevidence: c\nconfidence: high\n</experience>")
	f.Fuzz(func(t *testing.T, in string) {
		r, err := Parse(in)
		if err != nil {
			if r.Experience != nil || r.NoEntry {
				t.Fatalf("refusal carried content for %q", in)
			}
			return
		}
		if r.NoEntry == (r.Experience != nil) {
			t.Fatalf("accepted result is neither exactly NO_ENTRY nor exactly one entry: %+v", r)
		}
		if strings.Contains(in, "MANVI-VERDICT") {
			t.Fatalf("an answer carrying the verdict marker was accepted: %q", in)
		}
		if e := r.Experience; e != nil {
			if e.Cue == "" || e.Lesson == "" || e.Evidence == "" {
				t.Fatalf("accepted an entry with an empty field: %+v", e)
			}
			switch e.Confidence {
			case "low", "medium", "high":
			default:
				t.Fatalf("accepted confidence %q", e.Confidence)
			}
			if len(r.Edits) == 0 {
				t.Fatal("accepted an entry with no attributed edits")
			}
		}
		for _, ed := range r.Edits {
			if !ed.Verdict.valid() {
				t.Fatalf("accepted verdict %q", ed.Verdict)
			}
		}
	})
}
