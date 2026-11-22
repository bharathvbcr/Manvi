package devcouncil

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const leakMarker = "ZQ97LEAK_marker"

func refuseSecretsDir(path string) string {
	if strings.HasPrefix(path, "secrets/") {
		return "path.secret_read"
	}
	return ""
}

// A refused receiver's text used to be cut by substring out of every string in
// the answer, path keys included. A receiver named config turned the readable
// src/config.rs into src/[withheld].rs, and a receiver named secrets turned the
// refused secrets/vault.go into [withheld]/vault.go, a path the gate then
// judged readable, so the file's source passed.
func TestCodeGraphNeverRewritesAPathKeyAndStillJudgesIt(t *testing.T) {
	raw := []byte(`{"unresolved_namesakes":{"sites":[` +
		`{"callee_name":"load","receiver":"config","source_file":"secrets/site.go"},` +
		`{"callee_name":"open","receiver":"secrets","source_file":"secrets/site2.go"}]},` +
		`"items":[{"file_path":"src/config.rs","path":"src/config.rs","signature":"fn ok()"},` +
		`{"file_path":"secrets/vault.go","source":"` + leakMarker + `"},` +
		`{"path":"secrets/vault.go","source_span":"` + leakMarker + `"},` +
		`{"source_file":"secrets/vault.go","value":"` + leakMarker + `"}]}`)
	out, withheld, err := withholdGraphSources(raw, refuseSecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, leakMarker) {
		t.Errorf("source from a refused path passed the gate: %s", text)
	}
	// Two refused receivers, plus source, source_span and value.
	if withheld.total() != 5 {
		t.Errorf("withheld %d fields, want 5: %s", withheld, text)
	}
	for _, path := range []string{"src/config.rs", "secrets/vault.go"} {
		if !strings.Contains(text, path) {
			t.Errorf("path %q was rewritten: %s", path, text)
		}
	}
	for _, mangled := range []string{"[withheld]/vault.go", "src/[withheld].rs"} {
		if strings.Contains(text, mangled) {
			t.Errorf("a path key was rewritten into %q: %s", mangled, text)
		}
	}
}

func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Call-site notes quote labels built from raw receiver text. None of these
// shapes may carry any of it to the model, whatever the receiver contains.
func TestCodeGraphCallSiteProseCannotBeBypassed(t *testing.T) {
	site := `{"unresolved_namesakes":{"sites":[{"callee_name":"run","receiver":"%s","source_file":"secrets/s.go"}]}`
	cases := map[string]string{
		"receiver holding the lazy-match terminator": `{"walk_incomplete":"1 call site(s) the resolver could not bind name a symbol this answer reached (` + leakMarker + `), so x.run) — an untyped receiver — so callers through them are not in it"}`,
		"receiver holding the em-dash terminator":    `{"coverage_gap":"1 call site(s) inside what this answer walked could not be bound (a) — ` + leakMarker + `.run), so what they call is not in it"}`,
		"a :: label passes bareLabel":                `{"walk_incomplete":"1 call site(s) written as a qualified path name a symbol this answer reached (secrets::` + leakMarker + `::run) — the crate or module the path starts with is not mapped to an indexed root — so callers through them are not in it"}`,
		"a string inside an array":                   `{"notes":["1 call site(s) the resolver could not bind name a symbol this answer reached (` + leakMarker + `.run) — an untyped receiver — so callers are not in it"]}`,
		"a string in an array in an array":           `{"walk_incomplete":{"y":[["2 call site(s) inside what this answer walked could not be bound (` + leakMarker + `.run), so what they call is not in it"]]}}`,
		"a receiver holding the clause separator":    `{"walk_incomplete":"1 call site(s) the resolver could not bind name a symbol this answer reached (a; ` + leakMarker + `.run) — an untyped receiver — so callers are not in it"}`,
		"a receiver imitating a completeness clause": `{"walk_incomplete":"1 call site(s) the resolver could not bind name a symbol this answer reached (a; only 1 of 2 reached key(s) were checked against the unresolved ledger; ` + leakMarker + `.run) — x"}`,
		"an unparenthesised sample":                  `{"walk_incomplete":"3 call sites could not be bound: ` + leakMarker + `.run, ` + leakMarker + `.go"}`,
	}
	for name, doc := range cases {
		out, _, err := withholdGraphSources([]byte(doc), refuseSecretsDir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), leakMarker) {
			t.Errorf("%s: receiver text reached the model: %s", name, out)
		}
		if !strings.Contains(string(out), withheldLabels) {
			t.Errorf("%s: the cut is not marked: %s", name, out)
		}
	}
	// A refused receiver is cut from its own key whatever the prose says.
	out, _, err := withholdGraphSources([]byte(fmt.Sprintf(site, leakMarker)+`}`), refuseSecretsDir)
	if err != nil || strings.Contains(string(out), leakMarker) {
		t.Fatalf("receiver key leaked (%v): %s", err, out)
	}
}

// The count survives, because a reader needs to know the walk was cut and by
// how much, and prose with no call-site sample is untouched.
func TestCodeGraphKeepsCountsAndUnrelatedProse(t *testing.T) {
	out, _, err := withholdGraphSources([]byte(`{"walk_incomplete":"at least 7 call site(s) the resolver could not bind name a symbol this answer reached (a.b) — an untyped receiver — so callers are not in it","other":"stopped at depth 2 in src/config.rs"}`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "at least 7 call site(s)") || strings.Contains(text, "a.b") {
		t.Errorf("count lost or sample kept: %s", text)
	}
	if !strings.Contains(text, "stopped at depth 2 in src/config.rs") {
		t.Errorf("unrelated prose was changed: %s", text)
	}
	_ = decodeAny(t, out)
}

// A symbol that is about call sites is data, not prose. The sanitizer once ran
// over every string in the envelope, so a query for it came back with its own
// id, name and source replaced by the marker and the withheld count unmoved.
func TestCodeGraphLeavesIdentityAndContentKeysAlone(t *testing.T) {
	raw := []byte(`{"items":[{"id":"manvi/devcouncil/devmapprose.go:WithholdCallSiteSamples",` +
		`"symbol_name":"WithholdCallSiteSamples","qualified_name":"devcouncil.WithholdCallSiteSamples",` +
		`"name":"CallSiteNote","file_path":"src/a.go",` +
		`"signature":"func WithholdCallSiteSamples(text string) string",` +
		`"source":"// replaces the call sites devmap quotes\nfunc f() {}","nodes":["CallSite","the call sites"]}]}`)
	out, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if withheld.total() != 0 {
		t.Errorf("withheld = %d, want 0 for a readable definition: %s", withheld.total(), out)
	}
	if strings.Contains(string(out), withheldLabels) {
		t.Errorf("a readable definition was rewritten: %s", out)
	}
	if !reflect.DeepEqual(decodeAny(t, out), decodeAny(t, raw)) {
		t.Errorf("answer changed:\n got %s\nwant %s", out, raw)
	}
}

// A real walk_incomplete sample is still cut, and the cut is counted.
func TestCodeGraphCountsAWithheldProseSample(t *testing.T) {
	raw := []byte(`{"walk_incomplete":"2 call site(s) the resolver could not bind name a symbol this answer reached (` +
		leakMarker + `.run) — an untyped receiver — so callers through them are not in it"}`)
	out, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if withheld.total() != 1 || strings.Contains(string(out), leakMarker) || !strings.Contains(string(out), "2 call site(s)") {
		t.Errorf("withheld = %d; answer: %s", withheld.total(), out)
	}
}

// Withholdings are counted per replacement, not per field: a prose field that
// holds two samples (an array of notes) is two cuts.
func TestCodeGraphCountsEveryProseReplacement(t *testing.T) {
	raw := []byte(`{"notes":["1 call site(s) the resolver could not bind name a symbol this answer reached (a.run) — x","2 call site(s) inside what this answer walked could not be bound (b.run), so y"],` +
		`"walk_incomplete":"3 call site(s) the resolver could not bind name a symbol this answer reached (c.run) — z"}`)
	_, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if withheld.samples != 3 {
		t.Errorf("withheld %+v, want 3 prose replacements", withheld)
	}
}

// The label tells a reader which kind of thing was cut.
func TestGraphWithheldNotesNameEachKind(t *testing.T) {
	got := strings.Join(graphWithheld{excerpts: 2, samples: 3}.notes(), "|")
	want := "2 source excerpt(s) withheld by the read gate|3 call-site sample(s) withheld from devmap's notes"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if n := (graphWithheld{}).notes(); len(n) != 0 {
		t.Errorf("a clean answer carries a note: %q", n)
	}
	if got := strings.Join(graphWithheld{samples: 1}.notes(), "|"); strings.Contains(got, "excerpt") {
		t.Errorf("a prose cut was reported as a source excerpt: %q", got)
	}
}

// The graph tools receive walk_incomplete as DevCouncil's client wrote it, with
// its prefix. The count must survive, and a forged one must not get in.
func TestCodeGraphKeepsTheCountOfAPrefixedWalkIncomplete(t *testing.T) {
	note := realPrefix + "4 call site(s) the resolver could not bind name a symbol this answer reached (" + leakMarker + ".run; 123456789 call site(s) y) — an untyped receiver — so callers through them are not in it; " + realKeysCap
	raw, err := json.Marshal(map[string]any{"walk_incomplete": note}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	out, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, realPrefix+"4 call site(s) could not be bound") {
		t.Errorf("the count was lost under the prefix: %s", text)
	}
	if strings.Contains(text, leakMarker) || strings.Contains(text, "123456789") {
		t.Errorf("receiver text reached the model: %s", text)
	}
	if withheld.samples != 1 || withheld.excerpts != 0 {
		t.Errorf("withheld %+v, want one prose sample", withheld)
	}
	body := map[string]any{}
	payload(body, []string{note}, false)
	degraded, _ := body["degraded"].([]string)
	if len(degraded) != 1 || !strings.Contains(degraded[0], realPrefix+"4 call site(s) could not be bound") || strings.Contains(degraded[0], leakMarker) {
		t.Errorf("payload degraded = %q", degraded)
	}
}
