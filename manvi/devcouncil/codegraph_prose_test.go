package devcouncil

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

const leakToken = "ZQ97LEAK_sk_live"

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
		`{"file_path":"secrets/vault.go","source":"` + leakToken + `"},` +
		`{"path":"secrets/vault.go","source_span":"` + leakToken + `"},` +
		`{"source_file":"secrets/vault.go","value":"` + leakToken + `"}]}`)
	out, withheld, err := withholdGraphSources(raw, refuseSecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, leakToken) {
		t.Errorf("source from a refused path passed the gate: %s", text)
	}
	// Two refused receivers, plus source, source_span and value.
	if withheld != 5 {
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
		"receiver holding the lazy-match terminator": `{"walk_incomplete":"1 call site(s) the resolver could not bind name a symbol this answer reached (` + leakToken + `), so x.run) — an untyped receiver — so callers through them are not in it"}`,
		"receiver holding the em-dash terminator":    `{"coverage_gap":"1 call site(s) inside what this answer walked could not be bound (a) — ` + leakToken + `.run), so what they call is not in it"}`,
		"a :: label passes bareLabel":                `{"walk_incomplete":"1 call site(s) written as a qualified path name a symbol this answer reached (secrets::` + leakToken + `::run) — the crate or module the path starts with is not mapped to an indexed root — so callers through them are not in it"}`,
		"a string inside an array":                   `{"notes":["1 call site(s) the resolver could not bind name a symbol this answer reached (` + leakToken + `.run) — an untyped receiver — so callers are not in it"]}`,
		"a string in an array in an array":           `{"x":{"y":[["2 call site(s) inside what this answer walked could not be bound (` + leakToken + `.run), so what they call is not in it"]]}}`,
		"an unparenthesised sample":                  `{"walk_incomplete":"3 call sites could not be bound: ` + leakToken + `.run, ` + leakToken + `.go"}`,
	}
	for name, doc := range cases {
		out, _, err := withholdGraphSources([]byte(doc), refuseSecretsDir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), leakToken) {
			t.Errorf("%s: receiver text reached the model: %s", name, out)
		}
		if !strings.Contains(string(out), withheldLabels) {
			t.Errorf("%s: the cut is not marked: %s", name, out)
		}
	}
	// A refused receiver is cut from its own key whatever the prose says.
	out, _, err := withholdGraphSources([]byte(fmt.Sprintf(site, leakToken)+`}`), refuseSecretsDir)
	if err != nil || strings.Contains(string(out), leakToken) {
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
