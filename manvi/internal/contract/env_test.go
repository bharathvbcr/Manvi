package contract

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The environment is an interface the flag catalogue does not cover. See env.go.

// envDoc is where every environment variable the harness reads is documented.
const envDoc = docRoot + "/docs/CLI_AND_CONFIGURATION.md"

// envExempt are platform conventions: names the operating system or the shell
// define, which this harness reads but does not own. The document still says
// what the harness does with the terminal ones; these four are not worth a row.
var envExempt = map[string]bool{
	"HOME":   true,
	"TMPDIR": true,
	"USER":   true,
	"PATH":   true,
	// llm/adaptertest is test support compiled as production code; TEST_KEY is
	// a placeholder credential name no operator ever sets.
	"TEST_KEY": true,
}

// knownDynamicEnvReads are the reads whose name is not a literal anywhere the
// scan can follow. Each one is named with the reason it is not a gap, so a new
// read of this kind is a decision made in review rather than a number that
// quietly went up. The key is the file and the argument as written.
var knownDynamicEnvReads = map[string]string{
	"mcp/client.go k":                    "the loop over an MCP server's EnvPassthrough list: names the operator wrote in that server's own manifest, forwarded to that server and never interpreted by the harness (documented under child processes)",
	"mcp/manager.go k":                   "the same EnvPassthrough names, read by Manager.SecretValues only to hand their values to the scrubber so a server echoing its token is redacted; nothing is configured by them",
	"cmd/manvi/main.go dcgrep.BinaryEnv": "a constant in DevCouncil's dc/dcgrep package (MANVI_GREP_BINARY), outside this module, so the scan cannot read it; documented in the binaries table",
}

func TestEveryEnvironmentVariableTheHarnessReadsIsDocumented(t *testing.T) {
	m, err := Load(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	scan := m.EnvReads()
	if len(scan.Reads) < 20 {
		t.Fatalf("the scan found only %d variable names; it is examining almost nothing", len(scan.Reads))
	}
	doc := readDoc(t, envDoc)

	for _, miss := range UndocumentedEnv(scan, doc, envExempt) {
		t.Errorf("%s is not documented in %s: add it to the environment variable tables, "+
			"or, if it is a platform convention, to envExempt with the reason", miss, rel(envDoc))
	}

	// Reads the scan could not turn into a name are reported, not ignored.
	t.Logf("%d variable names, %d forwarded allowlists, %d reads with a name that is not a literal",
		len(scan.Reads), len(scan.Forwarded), len(scan.Unresolved))
	listed := map[string]bool{}
	for _, site := range scan.Unresolved {
		key := strings.SplitN(site.Where, ":", 2)[0] + " " + site.Expr
		listed[key] = true
		if _, ok := knownDynamicEnvReads[key]; !ok {
			t.Errorf("%s reads an environment variable named by %q, which the scan cannot resolve to a literal; "+
				"make the name a constant or a literal list, or add %q to knownDynamicEnvReads with the reason it is safe",
				site.Where, site.Expr, key)
		}
	}
	var stale []string
	for key := range knownDynamicEnvReads {
		if !listed[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("knownDynamicEnvReads lists %q, but no such read exists any more; remove it", key)
	}
}

// writeEnvFixture lays down a one-file module and returns its parsed form.
func writeEnvFixture(t *testing.T, source string) *Module {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/fixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTheEnvScanFindsEveryWayTheSourceReadsAName(t *testing.T) {
	m := writeEnvFixture(t, `package main

import "os"

const viaConst = "FIXTURE_FROM_CONST"

var passthroughAllowlist = []string{"FIXTURE_FORWARDED"}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	_ = os.Getenv("FIXTURE_DIRECT")
	_, _ = os.LookupEnv(viaConst)
	_ = env("FIXTURE_THROUGH_WRAPPER", "")
	for _, k := range []string{"FIXTURE_LOOP_A", "FIXTURE_LOOP_B"} {
		_ = os.Getenv(k)
	}
	for _, k := range passthroughAllowlist {
		_ = os.Getenv(k)
	}
	_ = "MANVI_FIXTURE_ONLY_A_LITERAL"
}

func dynamic(names map[string]string) {
	_ = os.Getenv(names["x"])
}
`)
	scan := m.EnvReads()
	got := map[string]bool{}
	for _, r := range scan.Reads {
		got[r.Name] = true
	}
	for _, want := range []string{"FIXTURE_DIRECT", "FIXTURE_FROM_CONST", "FIXTURE_THROUGH_WRAPPER", "FIXTURE_LOOP_A", "FIXTURE_LOOP_B", "MANVI_FIXTURE_ONLY_A_LITERAL"} {
		if !got[want] {
			t.Errorf("the scan missed %s; found %v", want, scan.Reads)
		}
	}
	if got["FIXTURE_FORWARDED"] {
		t.Errorf("a forwarded allowlist member was reported as an individual name")
	}
	if len(scan.Forwarded) != 1 || scan.Forwarded[0].Var != "passthroughAllowlist" {
		t.Errorf("forwarded allowlists = %v, want passthroughAllowlist", scan.Forwarded)
	}
	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Expr != "names[...]" {
		t.Errorf("unresolved = %v, want exactly the map lookup", scan.Unresolved)
	}
}

func TestANewUndocumentedReadFailsTheCheck(t *testing.T) {
	m := writeEnvFixture(t, `package main

import "os"

func main() { _ = os.Getenv("MANVI_NEW_THING") }
`)
	scan := m.EnvReads()
	missing := UndocumentedEnv(scan, "| `MANVI_SOMETHING_ELSE` | documented |", envExempt)
	if len(missing) != 1 || !strings.Contains(missing[0], "MANVI_NEW_THING") {
		t.Fatalf("an undocumented MANVI_NEW_THING must be reported, got %v", missing)
	}
	if got := UndocumentedEnv(scan, "`MANVI_NEW_THING_TWO` is not the same variable", envExempt); len(got) != 1 {
		t.Fatalf("a longer name must not satisfy a shorter one, got %v", got)
	}
	if got := UndocumentedEnv(scan, "| `MANVI_NEW_THING` | now documented |", envExempt); len(got) != 0 {
		t.Fatalf("a documented name must pass, got %v", got)
	}
}
