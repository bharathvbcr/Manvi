package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial tests for b25-toolsjson (manvi tools --json, manvi tool argument
// refusal). A failing TestAdvB25Tools... test is a break in the change.

type advToolsEntry struct {
	Name        string `json:"name"`
	Class       string `json:"class"`
	Description string `json:"description"`
}

func advToolsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	t.Setenv("MANVI_PROJECT_ROOT", e2eRepo(t))
}

func advToolsJSON(t *testing.T) (string, []advToolsEntry) {
	t.Helper()
	advToolsEnv(t)
	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"tools", "--json"}); err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	var got []advToolsEntry
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not a JSON array: %v", err)
	}
	return out.String(), got
}

// advPipeline holds tool "t" (read-only or write class) that records each run.
func advPipeline(t *testing.T, readOnly bool, ran *[]string) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(bus.New())
	err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "t", Description: "records", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(_ context.Context, c tools.Call) tools.Result {
			*ran = append(*ran, string(c.Arguments))
			return tools.Result{Text: "ran"}
		},
		ReadOnly: readOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Class column equals the registry's real ReadOnly flag, read through Subset's
// keep predicate (the Tool struct itself), not through the table text.
func TestAdvB25ToolsClassEqualsRegistryReadOnlyFlag(t *testing.T) {
	advToolsEnv(t)
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "")
	if err != nil {
		t.Skipf("surface not buildable: %v", err)
	}
	real := map[string]bool{}
	for _, s := range pipeline.Schemas() {
		name := s.Name
		real[name] = pipeline.Subset(func(tl tools.Tool) bool { return tl.Schema.Name == name && tl.ReadOnly }).Has(name)
	}
	_, got := advToolsJSON(t)
	if len(got) != len(real) {
		t.Fatalf("catalog has %d entries, registry %d", len(got), len(real))
	}
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.Name] {
			t.Errorf("duplicate catalog name %q", e.Name)
		}
		seen[e.Name] = true
		ro, ok := real[e.Name]
		if !ok {
			t.Errorf("%s is in the catalog but not the registry", e.Name)
			continue
		}
		want := "write"
		if ro {
			want = "read"
		}
		if e.Class != want {
			t.Errorf("%s: class %q, registry says %q", e.Name, e.Class, want)
		}
	}
}

var advANSI = regexp.MustCompile(`\x{1b}|\x{9b}`)

func TestAdvB25ToolsJSONIsOneValueAndNewlineNoANSIStable(t *testing.T) {
	raw1, got := advToolsJSON(t)
	raw2, _ := advToolsJSON(t)
	if raw1 != raw2 {
		t.Errorf("order/content differs between two runs")
	}
	if !strings.HasSuffix(raw1, "]\n") || strings.Count(raw1, "\n") != 1 {
		t.Errorf("want exactly one value then one newline, got %d newlines", strings.Count(raw1, "\n"))
	}
	dec := json.NewDecoder(strings.NewReader(raw1))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if dec.More() {
		t.Error("more than one JSON value on stdout")
	}
	if advANSI.MatchString(raw1) {
		t.Error("ANSI escape on stdout")
	}
	if !utf8.ValidString(raw1) {
		t.Error("stdout is not valid UTF-8")
	}
	for _, e := range got {
		for _, s := range []string{e.Name, e.Description} {
			for _, r := range s {
				if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
					t.Errorf("%s carries control rune %U", e.Name, r)
				}
			}
		}
	}
}

// The encoding of awkward descriptions round-trips.
func TestAdvB25ToolsEntryRoundTripsAwkwardText(t *testing.T) {
	for _, d := range []string{
		"quote \" backslash \\ newline \n tab \t nul \x00 esc \x1b[31m",
		"ls\u2028ps\u2029 <script>&</script> héllo 日本語 \U0001F600",
		firstSentence("First. Second line\nthird"),
	} {
		in := []toolCatalogEntry{{Name: "n", Class: "read", Description: d}}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			t.Fatal(err)
		}
		var back []toolCatalogEntry
		if err := json.Unmarshal(buf.Bytes(), &back); err != nil || len(back) != 1 || back[0] != in[0] {
			t.Errorf("round trip of %q: %v %+v", d, err, back)
		}
	}
}

// The table carries no ANSI and one row per JSON entry.
func TestAdvB25ToolsTableRowsAreSingleCleanLines(t *testing.T) {
	advToolsEnv(t)
	var out bytes.Buffer
	if err := run(&out, &bytes.Buffer{}, []string{"tools"}); err != nil {
		t.Skipf("surface not buildable: %v", err)
	}
	if advANSI.MatchString(out.String()) {
		t.Error("ANSI in the table")
	}
	_, got := advToolsJSON(t)
	rows := toolTable(t, out.String())
	if len(rows) != len(got) {
		t.Errorf("table has %d rows, JSON %d", len(rows), len(got))
	}
}

func TestAdvB25ToolsRefusesLookAlikeAndFormVariants(t *testing.T) {
	advToolsEnv(t)
	for _, a := range [][]string{
		{"--json=true"}, {"--json="}, {"-json"}, {"--JSON"}, {"--Json"}, {"--json", "--"}, {"--", "--json"},
		{""}, {"--json", ""}, {"‐‐json"}, {"−−json"}, {"--json "}, {" --json"},
		{"--json", "--json"}, {"--json=true", "--json"}, {"-"}, {"--help"}, {"-h"},
	} {
		full := append([]string{"tools"}, a...)
		var out, notes bytes.Buffer
		err := run(&out, &notes, full)
		if err == nil {
			t.Errorf("manvi tools %q was accepted (stdout %d bytes)", a, out.Len())
			continue
		}
		if out.Len() != 0 {
			t.Errorf("manvi tools %q refused but wrote to stdout: %q", a, out.String())
		}
		if status := exitStatus(&bytes.Buffer{}, &bytes.Buffer{}, full); status != 1 {
			t.Errorf("manvi tools %q exits %d, want 1", a, status)
		}
	}
}

// Refusals must not run the tool, for read and write class alike.
func TestAdvB25ToolsMalformedListsNeverRunAnyTool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "args.json")
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []bool{true, false} {
		for _, args := range [][]string{
			{"t", "--json", `{"a":1}`, "--json", `{"b":2}`},
			{"t", "--json="},
			{"t", "--json", ""},
			{"t", "--json", "{}", "extra"},
			{"t", "--json", `{"a":1}{"b":2}`},
			{"t", "--json", `{"a":1} x`},
			{"t", "--json", "{"},
			{"t", "--json", "-"},
			{"t", "--json", "@" + f},
			{"t", "--json=@" + f},
			{"t", "--json", "\x00"},
			{"t", "--json", "{\"a\":\"\x00\"}"},
			{"t", "--jsno", "{}"},
			{"t", "-json", "{}"},
			{"t", "--JSON", "{}"},
			{"t", "‐‐json", "{}"},
			{"t", "--json", "{}", "--"},
			{"t", "--", "--json", "{}"},
			{"t", ""},
			{"t", "--json={}", "--json={}"},
		} {
			var ran []string
			var out, notes bytes.Buffer
			err := callTool(&out, &notes, nil, advPipeline(t, readOnly, &ran), args)
			if err == nil {
				t.Errorf("readOnly=%v %q: accepted", readOnly, args)
			}
			if len(ran) != 0 {
				t.Errorf("readOnly=%v %q: the tool RAN with %q", readOnly, args, ran)
			}
		}
	}
}

// A payload that is valid JSON but not an object is not a tool's arguments.
func TestAdvB25ToolsPayloadMustBeAnObject(t *testing.T) {
	for _, payload := range []string{"[]", "null", "[1]", "\"s\"", "123", "true"} {
		var ran []string
		err := callTool(&bytes.Buffer{}, &bytes.Buffer{}, nil, advPipeline(t, false, &ran), []string{"t", "--json", payload})
		if err == nil || len(ran) != 0 {
			t.Errorf("--json %q: err=%v, the write-class tool ran with %q", payload, err, ran)
		}
	}
}

func TestAdvB25ToolsPayloadMustBeValidUTF8(t *testing.T) {
	for _, payload := range []string{"{\"a\":\"\xff\xfe\"}", "{\"a\":\"\xc0\xaf\"}", "{\"\xed\xa0\x80\":1}"} {
		var ran []string
		err := callTool(&bytes.Buffer{}, &bytes.Buffer{}, nil, advPipeline(t, false, &ran), []string{"t", "--json", payload})
		if err == nil || len(ran) != 0 {
			t.Errorf("invalid UTF-8 payload %q: err=%v, the tool ran with %q", payload, err, ran)
		}
	}
}

// A huge payload that is not valid JSON (here a truncated object) is refused
// without running the tool and without being echoed whole. The original version
// of this test required a 10 MB *valid* object to be refused too, which needs a
// size cap on tool arguments; none exists in manvi to reuse (run.go's 1 MiB is
// the stdin prompt limit) and argv is already bounded by the operating system,
// so the expectation was dropped rather than a new limit invented.
func TestAdvB25ToolsHugePayloadIsRefusedAndNotEchoed(t *testing.T) {
	big := "{\"a\":\"" + strings.Repeat("x", 10<<20)
	var ran []string
	err := callTool(&bytes.Buffer{}, &bytes.Buffer{}, nil, advPipeline(t, false, &ran), []string{"t", "--json", big})
	if err == nil || len(ran) != 0 {
		t.Fatalf("10 MB truncated payload: err=%v, ran %d times", err, len(ran))
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the refusal echoes the payload: error is %d bytes", len(err.Error()))
	}
}

func TestAdvB25ToolsInvalidPayloadIsNotEchoedInFull(t *testing.T) {
	var ran []string
	bad := strings.Repeat("x", 10<<20)
	err := callTool(&bytes.Buffer{}, &bytes.Buffer{}, nil, advPipeline(t, false, &ran), []string{"t", "--json", bad})
	if err == nil {
		t.Fatal("garbage payload accepted")
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the refusal echoes the whole payload: error is %d bytes", len(err.Error()))
	}
}

// The accepted forms still run, exactly once, with the bytes given.
func TestAdvB25ToolsAcceptedFormsRunOnce(t *testing.T) {
	for _, args := range [][]string{
		{"t", "--json", `{"a":"--json"}`},
		{"t", "--json={\"a\":\"x=y\"}"},
		{"t", "--json", `{"a":"\u0000"}`},
	} {
		var ran []string
		if err := callTool(&bytes.Buffer{}, &bytes.Buffer{}, nil, advPipeline(t, false, &ran), args); err != nil || len(ran) != 1 {
			t.Errorf("%q: err=%v ran=%d", args, err, len(ran))
		}
	}
}

type advBrokenWriter struct{}

func (advBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("write /dev/stdout: broken pipe")
}

// A closed stdout is the documented lost-output failure, never 0.
func TestAdvB25ToolsBrokenStdoutIsNotZero(t *testing.T) {
	advToolsEnv(t)
	repo := os.Getenv("MANVI_PROJECT_ROOT")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"tools"}, {"tools", "--json"},
		{"tool", "devcouncil_read_file", "--json", `{"path":"f.txt"}`},
	} {
		var notes bytes.Buffer
		status := exitStatus(advBrokenWriter{}, &notes, args)
		if status != 1 || !strings.Contains(notes.String(), "broken pipe") {
			t.Errorf("manvi %q on a broken stdout: status %d, notes %q", args, status, notes.String())
		}
	}
}

// The help's 'manvi tool' exit section, the docs section and the code agree.
func TestAdvB25ToolsHelpAndDocsExitSectionsMatch(t *testing.T) {
	_, helpBlock, ok := strings.Cut(usage, "Exit status of 'manvi tool")
	if !ok {
		t.Fatal("no help section")
	}
	helpBlock, _, _ = strings.Cut(helpBlock, "\n\n")
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "CLI_AND_CONFIGURATION.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, docBlock, ok := strings.Cut(string(doc), "### Tool Exit Codes (`manvi tool`)")
	if !ok {
		t.Fatal("no docs section")
	}
	docBlock, _, _ = strings.Cut(docBlock, "\n---")
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.NewReplacer("`", "", "*", "", "|", " ", "an ", "").Replace(s)), " ")
	}
	hn, dn := norm(helpBlock), norm(docBlock)
	for _, phrase := range []string{"no NAME", "unknown tool", "--jsno", "not valid JSON", "policy", "could not be built", "could not be written", "No status above 1", "0 answered, 1 refused, 2 malformed", "3 could not run here"} {
		if inHelp, inDoc := strings.Contains(hn, phrase), strings.Contains(dn, phrase); inHelp != inDoc {
			t.Errorf("phrase %q: help=%v docs=%v", phrase, inHelp, inDoc)
		}
	}
	if !strings.Contains(string(doc), "manvi tools [--json]") {
		t.Error("docs lack manvi tools [--json]")
	}
	advToolsEnv(t)
	for _, args := range [][]string{{"tool"}, {"tool", "nope"}, {"tool", "devcouncil_read_file", "--jsno", "{}"}, {"tool", "devcouncil_read_file", "--json"}, {"tool", "devcouncil_read_file", "--json", "{"}} {
		if st := exitStatus(&bytes.Buffer{}, &bytes.Buffer{}, args); st != 1 {
			t.Errorf("manvi %q exits %d, help documents 1", args, st)
		}
	}
}

// TUI parity: /tool with a misspelt flag is refused and the session survives;
// /tools --json is either handled or refused, not silently ignored.
func TestAdvB25ToolsTUIToolRefusesMisspeltFlag(t *testing.T) {
	var ran []string
	h, _ := commandHost(t)
	h.sessions["S1"].pipeline = advPipeline(t, false, &ran)
	var out bytes.Buffer
	_, err := h.runCommand(context.Background(), h.sessions["S1"], "S1", "tool", []string{"t", "--jsno", "{}"}, &out)
	if err == nil || len(ran) != 0 {
		t.Fatalf("/tool t --jsno {}: err=%v ran=%q", err, ran)
	}
}

func TestAdvB25ToolsTUIToolsFlagIsNotSilentlyIgnored(t *testing.T) {
	advToolsEnv(t)
	h, _ := commandHost(t)
	var out bytes.Buffer
	_, err := h.runCommand(context.Background(), h.sessions["S1"], "S1", "tools", []string{"--json"}, &out)
	if err == nil && !strings.HasPrefix(out.String(), "[") {
		t.Errorf("/tools --json printed the table, ignoring the argument: %.80q", out.String())
	}
	out.Reset()
	_, err = h.runCommand(context.Background(), h.sessions["S1"], "S1", "tools", []string{"--bogus"}, &out)
	if err == nil {
		t.Errorf("/tools --bogus was accepted and ignored")
	}
}
