package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// toolTable reads the name and read/write column of each row of the `manvi
// tools` table, in order.
func toolTable(t *testing.T, text string) [][2]string {
	t.Helper()
	var rows [][2]string
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break // the table ends at the blank line before the count
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("table row has no class column: %q", line)
		}
		rows = append(rows, [2]string{fields[0], fields[1]})
	}
	return rows
}

func TestToolsJSONIsTheTableAsAnArray(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	t.Setenv("MANVI_PROJECT_ROOT", e2eRepo(t))

	var table bytes.Buffer
	if err := run(&table, &bytes.Buffer{}, []string{"tools"}); err != nil {
		t.Fatalf("the native tool surface could not be built here, so this test examined nothing: %v", err)
	}
	var out bytes.Buffer
	if err := run(&out, &bytes.Buffer{}, []string{"tools", "--json"}); err != nil {
		t.Fatalf("manvi tools --json: %v", err)
	}
	var got []struct {
		Name        string `json:"name"`
		Class       string `json:"class"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("manvi tools --json is not a JSON array: %v\n%s", err, out.String())
	}
	rows := toolTable(t, table.String())
	if len(got) == 0 || len(got) != len(rows) {
		t.Fatalf("JSON has %d tools, the table has %d", len(got), len(rows))
	}
	for i, e := range got {
		if e.Name != rows[i][0] || e.Class != rows[i][1] {
			t.Errorf("entry %d is %s/%s, the table row is %s/%s", i, e.Name, e.Class, rows[i][0], rows[i][1])
		}
		if e.Class != "read" && e.Class != "write" {
			t.Errorf("%s has class %q, want read or write", e.Name, e.Class)
		}
		if e.Description == "" {
			t.Errorf("%s has no description", e.Name)
		}
	}
}

func TestToolsWithoutAFlagStillPrintsTheTable(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	t.Setenv("MANVI_PROJECT_ROOT", e2eRepo(t))

	var viaRun, direct bytes.Buffer
	if err := run(&viaRun, &bytes.Buffer{}, []string{"tools"}); err != nil {
		t.Fatalf("the native tool surface could not be built here, so this test examined nothing: %v", err)
	}
	if err := listTools(&direct, newTestRegistry(t)); err != nil {
		t.Fatal(err)
	}
	if viaRun.String() != direct.String() {
		t.Errorf("manvi tools changed:\n%s\nwant:\n%s", viaRun.String(), direct.String())
	}
	if !strings.Contains(viaRun.String(), " tools, all native") || strings.HasPrefix(viaRun.String(), "[") {
		t.Errorf("manvi tools is not the table:\n%s", viaRun.String())
	}
}

func TestToolsRefusesAnUnknownArgument(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	for _, args := range [][]string{
		{"tools", "--bogus"},
		{"tools", "extra"},
		{"tools", "--json", "extra"},
		{"tools", "--json", "--json"},
	} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, args); err == nil {
			t.Errorf("manvi %v succeeded:\n%s", args, out.String())
		}
		if status := exitStatus(&bytes.Buffer{}, &bytes.Buffer{}, args); status == 0 {
			t.Errorf("manvi %v exits 0", args)
		}
	}
}

// `manvi tool` has two statuses, and the help names both. The failures it
// folds into 1 are driven through exitStatus so the help is checked against
// what the process does, not against another sentence.
func TestHelpNamesEachExitStatusOfManviTool(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	t.Setenv("MANVI_PROJECT_ROOT", e2eRepo(t))

	_, block, ok := strings.Cut(usage, "Exit status of 'manvi tool")
	if !ok {
		t.Fatal("the help has no 'Exit status of manvi tool' section")
	}
	block, _, _ = strings.Cut(block, "\n\n")
	for _, want := range []string{"\n  0  ", "\n  1  ", "dev tool call"} {
		if !strings.Contains(block, want) {
			t.Errorf("the manvi tool exit section does not contain %q:\n%s", want, block)
		}
	}
	for status := 2; status <= 9; status++ {
		if strings.Contains(block, "\n  "+string(rune('0'+status))+"  ") {
			t.Errorf("the section documents status %d, which manvi tool never returns", status)
		}
	}

	for _, args := range [][]string{
		{"tool"},
		{"tool", "no_such_tool"},
		{"tool", "devcouncil_read_file", "--json", "{not json"},
	} {
		var out, notes bytes.Buffer
		if status := exitStatus(&out, &notes, args); status != 1 {
			t.Errorf("manvi %v exits %d, the help says 1", args, status)
		}
	}
}

// recordingPipeline holds one tool, "t", that records the arguments each call
// ran with.
func recordingPipeline(t *testing.T, ran *[]string) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(bus.New())
	err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "t", Description: "records its arguments", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(_ context.Context, c tools.Call) tools.Result {
			*ran = append(*ran, string(c.Arguments))
			return tools.Result{Text: "ran"}
		},
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// A tool must never run on arguments the caller did not give it: a misspelt
// flag, a --json with no value, a second --json and a stray word are all
// refused, with the usage and the ordinary failure status, before the tool runs.
func TestManviToolRefusesMalformedArgumentsInsteadOfRunningWithNone(t *testing.T) {
	for _, args := range [][]string{
		{"t", "--jsno", "{}"},
		{"t", "--json"},
		{"t", "extra"},
		{"t", "--bogus"},
		{"t", "--json", "{}", "extra"},
		{"t", "--json", "{}", "--json", "{}"},
		{"t", "--json={}", "extra"},
	} {
		var ran []string
		var out, notes bytes.Buffer
		err := callTool(&out, &notes, nil, recordingPipeline(t, &ran), args)
		if err == nil || !strings.Contains(err.Error(), "usage: manvi tool NAME") {
			t.Errorf("manvi tool %q: error = %v, want the usage", args, err)
		}
		if len(ran) != 0 {
			t.Errorf("manvi tool %q ran the tool with %q", args, ran)
		}
		if status := statusFor(err, &notes); status != 1 {
			t.Errorf("manvi tool %q exits %d, want 1", args, status)
		}
	}
}

func TestManviToolStillRunsWithWellFormedArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"t"}, "{}"},
		{[]string{"t", "--json", `{"a":1}`}, `{"a":1}`},
		{[]string{"t", `--json={"a":1}`}, `{"a":1}`},
	} {
		var ran []string
		var out, notes bytes.Buffer
		if err := callTool(&out, &notes, nil, recordingPipeline(t, &ran), tc.args); err != nil {
			t.Fatalf("manvi tool %q: %v", tc.args, err)
		}
		if len(ran) != 1 || ran[0] != tc.want {
			t.Errorf("manvi tool %q ran with %q, want one run with %s", tc.args, ran, tc.want)
		}
	}
}
