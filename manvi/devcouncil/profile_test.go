package devcouncil

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/llm"
)

// The two invariants exercised in this file exist because `llm.local.
// core_tools_only` shipped violating both, and the violation was a dead end
// rather than a degradation.
//
// The flag drops every tool marked Extended. `devcouncil_policy_check_write`
// and `devcouncil_verify_task` were core but reached a lease check, and the
// only tool that takes a lease — `devcouncil_checkout_task` — was Extended. So
// the reduced surface offered two tools that could never succeed, and every
// refusal they produced named tools the model had not been given. Under the
// strict posture that closed the loop entirely: writes were refused for
// `task.absent`, the refusal pointed at `devcouncil_checkout_task`, and the
// only alternative it named was `devcouncil_request_override` — also removed.
//
// Both tests are written over the registry and over the package's own source
// rather than over a list of tool pairs, because a hand-written list is exactly
// what stops being true the next time a tool is added.

// coreNames is the set of tools a core-profile model can actually call.
func coreNames(f *fixture) map[string]bool {
	f.t.Helper()
	return schemaNames(f.pipe.CoreSchemas())
}

func schemaNames(schemas []llm.ToolSchema) map[string]bool {
	out := map[string]bool{}
	for _, s := range schemas {
		out[s.Name] = true
	}
	return out
}

func allNames(f *fixture) []string {
	f.t.Helper()
	var out []string
	for _, s := range f.pipe.Schemas() {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// TestNoToolNamesAToolTheCoreProfileDoesNotOffer reads the package's own source
// for every tool name it puts in front of a model.
//
// A string literal naming a tool is only ever written here for one reason: to
// route an agent somewhere after a refusal. Naming a tool the agent has not
// been offered is worse than naming nothing — it sends the model to call
// something that will come back "unknown tool", and it does so at exactly the
// moment the model is already stuck.
//
// Definition sites are excluded: `schema("devcouncil_get_gaps", …)` declares a
// tool rather than referring to one, and an Extended tool is entitled to
// declare itself. Everything else is a reference, and every reference must
// resolve inside the smallest profile the harness offers.
func TestNoToolNamesAToolTheCoreProfileDoesNotOffer(t *testing.T) {
	f := newFixture(t)
	core := coreNames(f)
	registered := schemaNames(f.pipe.Schemas())

	fset := token.NewFileSet()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package source: %v", err)
	}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}

		// Definition sites: the first argument of every schema(...) call. A
		// tool is entitled to declare its own name whatever profile it is in.
		defined := map[token.Pos]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			if ident, isIdent := call.Fun.(*ast.Ident); !isIdent || ident.Name != "schema" {
				return true
			}
			if lit, isLit := call.Args[0].(*ast.BasicLit); isLit {
				defined[lit.Pos()] = true
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING || defined[lit.Pos()] {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, name := range toolNamesIn(value) {
				line := fset.Position(lit.Pos()).Line
				if !registered[name] {
					t.Errorf("%s:%d names %q, which is not a registered tool", path, line, name)
					continue
				}
				if !core[name] {
					t.Errorf("%s:%d names %q, which the core profile does not offer; "+
						"a model running with llm.local.core_tools_only cannot call it",
						path, line, name)
				}
			}
			return true
		})
	}
}

// toolNamesIn pulls every devcouncil_* token out of a string, so a name
// embedded in a sentence is found as readily as one that is the whole literal.
func toolNamesIn(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		idx := strings.Index(s[i:], "devcouncil_")
		if idx < 0 {
			break
		}
		start := i + idx
		end := start
		for end < len(s) && (s[end] == '_' || (s[end] >= 'a' && s[end] <= 'z') || (s[end] >= 'A' && s[end] <= 'Z')) {
			end++
		}
		out = append(out, s[start:end])
		i = end
	}
	return out
}

// TestTheCoreProfileHasARouteOutOfEveryRefusal is the same invariant driven
// through the tools rather than read out of the source: take the posture that
// refuses the most, hold no lease, and check that nothing an agent is told
// mentions a tool it was not given.
//
// The measured failure this pins: under harness.posture=strict with
// core_tools_only on, devcouncil_write_file returned
// {"suggested_tool":"devcouncil_checkout_task", …} and
// devcouncil_policy_check_write returned "no task is checked out; call
// devcouncil_checkout_task first" — both naming a tool the flag had removed.
func TestTheCoreProfileHasARouteOutOfEveryRefusal(t *testing.T) {
	f := newFixture(t)
	core := coreNames(f)
	every := allNames(f)

	// No checkout: this is the state the flag leaves an agent in when it has
	// no way to leave it.
	for _, probe := range []struct {
		tool string
		args any
	}{
		{"devcouncil_policy_check_write", map[string]string{"path": "src/calc.go"}},
		{"devcouncil_write_file", map[string]string{"path": "docs/notes.md", "content": "x"}},
		{"devcouncil_verify_task", map[string]any{}},
	} {
		result := f.call(probe.tool, probe.args)
		for _, name := range every {
			if !strings.Contains(result.Text, name) || core[name] {
				continue
			}
			t.Errorf("%s named %q, which the core profile does not offer: %s",
				probe.tool, name, result.Text)
		}
	}
}

// TestPolicyCheckAnswersWhatTheWriteWouldAnswer is the disagreement underneath
// the dead end.
//
// devcouncil_policy_check_write exists to preview devcouncil_write_file without
// performing it, so the two must reach the same verdict from the same state.
// They did not: the write asked the gate with a nil task and let the posture
// decide, while the preview refused up front on the missing lease. Under the
// shipped dev posture — and under yolo, whose entire meaning is that the gate is
// not containing the agent — the write succeeded and the preview reported a
// hard stop for the same path. An agent that trusts the cheaper call is told
// the work is impossible when it is not.
func TestPolicyCheckAnswersWhatTheWriteWouldAnswer(t *testing.T) {
	for _, posture := range []string{flags.PostureDev, flags.PostureStrict, flags.PostureYolo} {
		t.Run(posture, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})

			// A preview that answers "no" is still an answer, so the two are
			// compared on their verdict rather than on IsError: what the
			// preview reports as `allowed` must be what the write then does.
			preview := f.call("devcouncil_policy_check_write", map[string]string{"path": "docs/notes.md"})
			if preview.IsError {
				t.Fatalf("the preview could not answer at all: %s", preview.Text)
			}
			var verdict struct {
				Allowed *bool `json:"allowed"`
			}
			if err := json.Unmarshal([]byte(preview.Text), &verdict); err != nil || verdict.Allowed == nil {
				t.Fatalf("the preview returned no verdict to compare: %v (%q)", err, preview.Text)
			}

			write := f.call("devcouncil_write_file", map[string]string{
				"path": "docs/notes.md", "content": "x",
			})
			if *verdict.Allowed == write.IsError {
				t.Fatalf("preview said allowed=%v but the write %s;\n  preview: %s\n  write:   %s",
					*verdict.Allowed,
					map[bool]string{true: "was refused", false: "succeeded"}[write.IsError],
					preview.Text, write.Text)
			}
		})
	}
}

// TestEveryProfileIsClosedUnderItsPrerequisites is the structural half.
//
// A tool declares what it needs through tools.Tool.Requires, and a profile that
// offers the tool must offer what it needs. Declaring it in the registry is
// what keeps this true as tools are added: a new tool that reaches held() says
// so once, and any profile that would strand it fails here rather than in front
// of a model.
func TestEveryProfileIsClosedUnderItsPrerequisites(t *testing.T) {
	f := newFixture(t)
	for _, profile := range []struct {
		label   string
		schemas []llm.ToolSchema
	}{
		{"all tools", f.pipe.Schemas()},
		{"core", f.pipe.CoreSchemas()},
	} {
		for _, gap := range f.pipe.UnsatisfiedIn(profile.schemas) {
			t.Errorf("%s profile: %s", profile.label, gap)
		}
	}
	// And a prerequisite naming a tool that does not exist is a typo that would
	// otherwise be invisible until the profile happened to omit it.
	if gaps := f.pipe.UnsatisfiedIn(f.pipe.Schemas()); len(gaps) != 0 {
		t.Errorf("the full surface cannot be missing anything: %v", gaps)
	}
}
