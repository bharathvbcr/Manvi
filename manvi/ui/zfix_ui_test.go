package ui

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"manvi/credentials"
)

// zfixKey is the value these tests watch. It is not a credential and never was.
const zfixKey = "sk-ant-FAKE-TESTVALUE-1234567890abcdef"

func armedScrubber() *credentials.Scrubber {
	s := credentials.NewScrubber()
	s.Watch(credentials.NewSecret(zfixKey, "ANTHROPIC_API_KEY"))
	return s
}

// TestTheJSONSinkScrubsEveryFieldNotTwoOfThem is defect 4.
//
// The sink cleaned Text and Detail and left about thirteen other fields alone,
// so one key echoed back by a provider reached the transcript through the tool
// call's arguments, the blocked path, the grant's author, and the list of checks
// that did not run — four copies in one line, on disk, under a doc comment that
// says "a credential in it is a credential on disk".
func TestTheJSONSinkScrubsEveryFieldNotTwoOfThem(t *testing.T) {
	var out bytes.Buffer
	sink := NewJSONSink(&out, armedScrubber())
	sink.Emit(Event{
		Kind:      KindToolResult,
		Agent:     "child " + zfixKey,
		Text:      "body " + zfixKey,
		Detail:    "detail " + zfixKey,
		Tool:      "http " + zfixKey,
		Arguments: json.RawMessage(`{"header":"Bearer ` + zfixKey + `","nested":{"k":"` + zfixKey + `"}}`),
		Rule:      "rule " + zfixKey,
		Severity:  "soft " + zfixKey,
		Path:      "/tmp/" + zfixKey,
		GrantID:   "G-" + zfixKey,
		GrantedBy: "human " + zfixKey,
		Demoted:   "demoted " + zfixKey,
		Degraded:  []string{"neighbour check: " + zfixKey},
		Weakened:  []string{"policy.file.mode: " + zfixKey},
		TaskID:    "T-" + zfixKey,
		Posture:   "dev " + zfixKey,
		Model:     "m-" + zfixKey,
	})

	line := out.String()
	if strings.Contains(line, zfixKey) {
		t.Fatalf("the JSON transcript carries the credential:\n%s", line)
	}
	if n := strings.Count(line, credentials.Redacted); n < 15 {
		t.Fatalf("only %d fields were redacted; every one carrying the value should be:\n%s", n, line)
	}
	// And the line is still JSON a consumer can read, which is the whole point
	// of not sanitizing here.
	var back Event
	if err := json.Unmarshal([]byte(line), &back); err != nil {
		t.Fatalf("the scrubbed line is not valid JSON: %v\n%s", err, line)
	}
	if back.Kind != KindToolResult {
		t.Fatalf("the event lost its kind: %q", back.Kind)
	}
}

// TestTheJSONSinkKeepsControlSequencesAndNumbersIntact.
//
// Escaping text here would corrupt a record meant for a program, and decoding a
// large integer through float64 on the way back out would corrupt it just as
// surely. Both are properties of a transcript nothing else can restore, so both
// are pinned.
func TestTheJSONSinkKeepsControlSequencesAndNumbersIntact(t *testing.T) {
	var out bytes.Buffer
	sink := NewJSONSink(&out, armedScrubber())
	sink.Emit(Event{
		Kind:      KindToolStart,
		Text:      "cleared \x1b[2J",
		Arguments: json.RawMessage(`{"id":12345678901234567890,"ratio":1.5,"flag":true,"none":null}`),
	})

	line := out.String()
	if !strings.Contains(line, `\u001b[2J`) {
		t.Fatalf("the escape was rewritten; the JSON face must keep it:\n%s", line)
	}
	if !strings.Contains(line, "12345678901234567890") {
		t.Fatalf("a large integer id did not survive:\n%s", line)
	}
}

// TestUnchangedArgumentsAreNotReEncoded: a document with nothing to remove is
// passed through byte for byte, key order included, so the common case keeps the
// exact fidelity this face promises.
func TestUnchangedArgumentsAreNotReEncoded(t *testing.T) {
	raw := json.RawMessage(`{"z":1,"a":"two",  "m":[3,{"n":null}]}`)
	got := CleanJSON(raw, armedScrubber().Clean)
	if string(got) != string(raw) {
		t.Fatalf("arguments were rewritten with nothing to remove\n got: %s\nwant: %s", got, raw)
	}
}

// TestUnparseableArgumentsArePassedThroughUnchanged.
//
// There is no string in them to reach, and both faces already refuse them: the
// terminal renders nothing for arguments it cannot parse, and encoding an
// invalid RawMessage fails. Inventing a replacement document would put bytes in
// the transcript the model never sent.
func TestUnparseableArgumentsArePassedThroughUnchanged(t *testing.T) {
	raw := json.RawMessage(`{not json`)
	if got := CleanJSON(raw, armedScrubber().Clean); string(got) != string(raw) {
		t.Fatalf("unparseable arguments were rewritten into %s", got)
	}
	if got := CleanJSON(nil, armedScrubber().Clean); got != nil {
		t.Fatalf("nil arguments became %q", got)
	}
}

// TestCleanJSONReachesKeysAndNestedValues: a credential used as an object key,
// or buried in an array of objects, is still a credential.
func TestCleanJSONReachesKeysAndNestedValues(t *testing.T) {
	raw := json.RawMessage(`{"` + zfixKey + `":[{"deep":"` + zfixKey + `"}]}`)
	got := string(CleanJSON(raw, armedScrubber().Clean))
	if strings.Contains(got, zfixKey) {
		t.Fatalf("the credential survived in %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("the result is not valid JSON: %s", got)
	}
}

// TestCleanEventReachesEveryUntrustedFieldOnTheType is the structural half of
// defects 1 and 4.
//
// Both were the same mistake: a list of fields written by hand, correct on the
// day it was written and quietly short of the type afterwards. The JSON sink
// named two of about fifteen. So this walks Event by reflection instead of
// naming fields, fills every string, string slice and raw-JSON field with a
// marker, and requires the cleaner to have reached all of them — which is a test
// a field added tomorrow fails without anyone remembering to update it.
//
// The exemptions are the fields the harness composes itself and a face keys its
// own behaviour off: Kind selects the renderer's branch, and rewriting it would
// change which one runs.
func TestCleanEventReachesEveryUntrustedFieldOnTheType(t *testing.T) {
	composed := map[string]string{
		"Kind": "the harness picks it, and every face switches on it",
	}

	const marker = "zqxmarkerzqx"
	var e Event
	v := reflect.ValueOf(&e).Elem()
	typ := v.Type()

	var filled []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, skip := composed[f.Name]; skip {
			continue
		}
		switch {
		case f.Type == reflect.TypeOf(json.RawMessage(nil)):
			v.Field(i).SetBytes([]byte(`{"k":"` + marker + `"}`))
			filled = append(filled, f.Name)
		case f.Type.Kind() == reflect.String:
			v.Field(i).SetString(marker)
			filled = append(filled, f.Name)
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String:
			v.Field(i).Set(reflect.ValueOf([]string{marker}))
			filled = append(filled, f.Name)
		}
	}
	if len(filled) < 15 {
		t.Fatalf("only %d fields were exercised (%v); the walk is not reaching the type", len(filled), filled)
	}

	got := CleanEvent(e, func(string) string { return "CLEANED" })
	out := reflect.ValueOf(got)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, skip := composed[f.Name]; skip {
			continue
		}
		field := out.Field(i)
		switch {
		case f.Type == reflect.TypeOf(json.RawMessage(nil)):
			if strings.Contains(string(field.Bytes()), marker) {
				t.Errorf("%s was not cleaned: %s", f.Name, field.Bytes())
			}
		case f.Type.Kind() == reflect.String:
			if strings.Contains(field.String(), marker) {
				t.Errorf("%s was not cleaned: %q", f.Name, field.String())
			}
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String:
			for j := 0; j < field.Len(); j++ {
				if strings.Contains(field.Index(j).String(), marker) {
					t.Errorf("%s[%d] was not cleaned: %q", f.Name, j, field.Index(j).String())
				}
			}
		}
	}
}

// TestCleanEventDoesNotMutateTheCallersSlices.
//
// An event fans out to several sinks. A cleaner that wrote through the Degraded
// slice would hand the second sink a value the first one had already rewritten,
// and on the terminal face that means escaped text reaching the JSON transcript.
func TestCleanEventDoesNotMutateTheCallersSlices(t *testing.T) {
	degraded := []string{"check: " + zfixKey}
	e := Event{Kind: KindPolicy, Degraded: degraded, Weakened: []string{zfixKey}}
	CleanEvent(e, armedScrubber().Clean)
	if degraded[0] != "check: "+zfixKey {
		t.Fatalf("the caller's slice was rewritten in place: %q", degraded[0])
	}
}

// TestATruncatedToolResultIsScrubbedBeforeItIsCut is defect 5.
//
// The renderer cut first and cleaned second. A credential straddling the
// boundary was therefore split, no longer matched the watched value, and the
// clear prefix of a real key was printed. The order is what fixes it: cleaning
// first means the whole value is present when the scrubber looks for it, and
// the cap then applies to the text that will actually be shown.
func TestATruncatedToolResultIsScrubbedBeforeItIsCut(t *testing.T) {
	for _, cap := range []int{20, 30, 40, 41, 50} {
		var out bytes.Buffer
		r := NewRenderer(&out, armedScrubber())
		r.SetPalette(PlainPalette())
		r.MaxToolResult = cap
		// The key starts inside the budget and ends outside it, which is the
		// straddle. Several caps, because the defect only shows for the ones
		// that land in the middle of the value.
		r.Emit(Event{Kind: KindToolResult, Text: "prefix " + zfixKey + " suffix"})

		body := out.String()
		if strings.Contains(body, zfixKey) {
			t.Fatalf("cap %d printed the whole credential:\n%s", cap, body)
		}
		// Any run of the key's own characters long enough to be useful is a
		// leak; eight is the same floor the scrubber uses to decide a value is
		// worth watching at all.
		for n := 8; n <= len(zfixKey); n++ {
			if strings.Contains(body, zfixKey[:n]) {
				t.Fatalf("cap %d printed a %d-character prefix of the credential:\n%s", cap, n, body)
			}
		}
	}
}

// TestATruncatedReasoningLineIsScrubbedBeforeItIsCut is the same defect at the
// other call site, which had the same shape and would have been left behind by
// a fix aimed at the one that was noticed.
func TestATruncatedReasoningLineIsScrubbedBeforeItIsCut(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, armedScrubber())
	r.SetPalette(PlainPalette())
	// 400 runes is the reasoning cap, so the key is placed to straddle it.
	filler := strings.Repeat("x", 400-len(zfixKey)/2)
	r.Emit(Event{Kind: KindReasoning, Text: filler + zfixKey + " tail"})

	body := out.String()
	for n := 8; n <= len(zfixKey); n++ {
		if strings.Contains(body, zfixKey[:n]) {
			t.Fatalf("a %d-character prefix of the credential was printed:\n%s", n, body)
		}
	}
}

// TestTruncationStillBoundsAndStillSaysSo: cleaning first must not lose the
// bound, and a cut result must still be marked, or a truncated tool result reads
// as a complete one.
func TestTruncationStillBoundsAndStillSaysSo(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, credentials.NewScrubber())
	r.SetPalette(PlainPalette())
	r.MaxToolResult = 32
	r.Emit(Event{Kind: KindToolResult, Text: strings.Repeat("y", 500)})

	body := out.String()
	if !strings.Contains(body, "[truncated]") {
		t.Fatalf("the cut was not marked:\n%s", body)
	}
	if strings.Count(body, "y") > 32 {
		t.Fatalf("the cap did not bound the output:\n%s", body)
	}
}

// TestTheRendererStillNeutralisesEscapesAfterTheReorder guards the property the
// reorder could have dropped: safe still runs, and it still runs on everything.
func TestTheRendererStillNeutralisesEscapesAfterTheReorder(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, credentials.NewScrubber())
	r.SetPalette(PlainPalette())
	r.Emit(Event{Kind: KindToolResult, Text: "a\x1b[2Jb"})
	r.Emit(Event{Kind: KindReasoning, Text: "a\x1b[2Jb"})
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatalf("a raw escape reached the line renderer's output:\n%q", out.String())
	}
}

// TestCleanRequestReachesEveryFieldTheCardDraws: the approval card draws the
// rule, the severity, the subject, the reason, the task and every choice, so a
// cleaner that missed one would leave the modal repaintable through it.
func TestCleanRequestReachesEveryFieldTheCardDraws(t *testing.T) {
	const bad = "\x1b[2J"
	req := CleanRequest(Request{
		ID: "A" + bad, Rule: "r" + bad, Severity: "soft" + bad,
		Path: "p" + bad, Reason: "why" + bad, TaskID: "T" + bad,
		Choices: []string{"one" + bad, "two" + bad},
	}, Sanitize)

	for name, got := range map[string]string{
		"id": req.ID, "rule": req.Rule, "severity": req.Severity,
		"path": req.Path, "reason": req.Reason, "task": req.TaskID,
		"choice 0": req.Choices[0], "choice 1": req.Choices[1],
	} {
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("%s still carries a raw escape: %q", name, got)
		}
	}
}
