package devcouncil

import (
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
