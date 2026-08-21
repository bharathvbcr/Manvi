package prompt

import (
	"errors"
	"strings"
	"testing"
)

// TestAssemblyIsDeterministic: two runs with the same inputs must produce the
// same bytes, or the prompt cannot be diffed across runs or cached by a
// provider.
func TestAssemblyIsDeterministic(t *testing.T) {
	build := func() string {
		a := New()
		if err := a.Add(
			Static("tools", 30, true, "You have these tools."),
			Static("identity", 10, true, "You are the builder."),
			Static("scope", 20, true, "Stay inside the task's planned files."),
			// Same order as scope: the tie must break by name, not by
			// registration order.
			Static("repo", 20, false, "This repository is Go and Rust."),
		); err != nil {
			t.Fatal(err)
		}
		text, _ := a.Assemble()
		return text
	}
	first, second := build(), build()
	if first != second {
		t.Fatal("assembly is not deterministic")
	}
	want := []string{"You are the builder.", "This repository", "Stay inside", "You have these tools."}
	at := -1
	for _, fragment := range want {
		i := strings.Index(first, fragment)
		if i < 0 {
			t.Fatalf("%q missing from:\n%s", fragment, first)
		}
		if i < at {
			t.Fatalf("sections are out of order at %q:\n%s", fragment, first)
		}
		at = i
	}
}

// TestAFailingSourceIsReportedNotSwallowed: a prompt assembled while a
// contributor was failing is not the prompt that was intended.
func TestAFailingSourceIsReportedNotSwallowed(t *testing.T) {
	a := New()
	if err := a.Add(
		Static("identity", 10, true, "You are the builder."),
		SourceFunc{Label: "repo-map", Fn: func() ([]Section, error) {
			return nil, errors.New("code graph not built")
		}},
	); err != nil {
		t.Fatal(err)
	}
	text, report := a.Assemble()

	// The run continues — refusing to build a prompt because one optional
	// contributor failed turns a degraded run into no run.
	if !strings.Contains(text, "You are the builder.") {
		t.Fatal("a failing source aborted the whole assembly")
	}
	if report.Complete() {
		t.Fatal("a prompt built while a contributor was failing must not report complete")
	}
	if len(report.Failed) != 1 || report.Failed[0].Source != "repo-map" {
		t.Fatalf("failed = %+v", report.Failed)
	}
	if !strings.Contains(report.Failed[0].Reason, "code graph not built") {
		t.Fatalf("the reason was lost: %+v", report.Failed[0])
	}
}

// TestAnEssentialSectionIsNeverTrimmedForBudget is the consequential rule. A
// model told it may write anywhere, because the scope section did not fit, is a
// model that will try.
func TestAnEssentialSectionIsNeverTrimmedForBudget(t *testing.T) {
	a := New()
	a.MaxRunes = 60
	if err := a.Add(
		Static("filler", 10, false, strings.Repeat("x", 55)),
		Static("scope", 20, true, "Never write outside the task's planned files."),
	); err != nil {
		t.Fatal(err)
	}
	text, report := a.Assemble()

	if !strings.Contains(text, "Never write outside") {
		t.Fatal("an essential section was dropped for budget")
	}
	if report.Complete() {
		t.Fatal("a prompt that blew its budget must say so")
	}
	// And the overrun is recorded, so nobody reads the prompt as within budget.
	var kept bool
	for _, o := range report.Omitted {
		if o.Name == "scope" && strings.Contains(o.Reason, "never trimmed") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the budget overrun was not recorded: %+v", report.Omitted)
	}
}

// TestANonEssentialSectionIsDroppedAndSaidSo.
func TestANonEssentialSectionIsDroppedAndSaidSo(t *testing.T) {
	a := New()
	a.MaxRunes = 40
	if err := a.Add(
		Static("identity", 10, true, "You are the builder."),
		Static("history", 20, false, strings.Repeat("h", 200)),
	); err != nil {
		t.Fatal(err)
	}
	text, report := a.Assemble()
	if strings.Contains(text, "hhh") {
		t.Fatal("the oversized optional section was included")
	}
	if len(report.Omitted) != 1 || !strings.Contains(report.Omitted[0].Reason, "budget") {
		t.Fatalf("omitted = %+v", report.Omitted)
	}
}

// TestAnEmptySectionIsRecordedNotSkipped: a contributor that produced nothing
// and a contributor that was never registered are different facts.
func TestAnEmptySectionIsRecordedNotSkipped(t *testing.T) {
	a := New()
	if err := a.Add(
		Static("identity", 10, true, "You are the builder."),
		Static("task", 20, true, "   "),
	); err != nil {
		t.Fatal(err)
	}
	_, report := a.Assemble()
	if len(report.Omitted) != 1 || report.Omitted[0].Name != "task" {
		t.Fatalf("omitted = %+v", report.Omitted)
	}
	if !strings.Contains(report.Omitted[0].Reason, "no content") {
		t.Fatalf("reason = %q", report.Omitted[0].Reason)
	}
}

// TestDuplicateSourceNamesAreRefused: two contributors under one name make the
// report ambiguous about which produced what, and the report is the point.
func TestDuplicateSourceNamesAreRefused(t *testing.T) {
	a := New()
	if err := a.Add(Static("identity", 10, true, "a")); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(Static("identity", 20, true, "b")); err == nil {
		t.Fatal("a duplicate source name was accepted")
	}
	if err := a.Add(Static("", 10, true, "x")); err == nil {
		t.Fatal("a nameless source was accepted")
	}
}

// TestEveryIncludedSectionIsAccountedFor: the report must add up, or it cannot
// answer "what did the model actually know".
func TestEveryIncludedSectionIsAccountedFor(t *testing.T) {
	a := New()
	if err := a.Add(
		Static("identity", 10, true, "You are the builder."),
		Static("scope", 20, true, "Stay in scope."),
		Static("tools", 30, true, "Use the tools."),
	); err != nil {
		t.Fatal(err)
	}
	text, report := a.Assemble()
	if len(report.Included) != 3 || !report.Complete() {
		t.Fatalf("report = %+v", report)
	}
	if report.Runes != len([]rune(text)) {
		t.Fatalf("report says %d runes, the prompt has %d", report.Runes, len([]rune(text)))
	}
	for _, inc := range report.Included {
		if inc.Runes == 0 || inc.Source == "" {
			t.Fatalf("an included section is not attributable: %+v", inc)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens(""); n != 0 {
		t.Errorf("expected 0 for empty string, got %d", n)
	}
	if n := EstimateTokens("hello world"); n < 2 {
		t.Errorf("expected at least 2 tokens, got %d", n)
	}
	codeSample := `func main() { fmt.Println("Hello, world!") }`
	if n := EstimateTokens(codeSample); n < 5 {
		t.Errorf("expected realistic token count for code, got %d", n)
	}
}

// A long unbroken run — base64, a hash, a minified one-liner — is many tokens
// to every real tokenizer. Counting it as one word read as "plenty of room"
// to the compaction planner and let an overflowing request out the door.
func TestEstimateTokensDoesNotCollapseOnUnbrokenRuns(t *testing.T) {
	blob := strings.Repeat("x", 2_000_000)
	if n := EstimateTokens(blob); n < 100_000 {
		t.Errorf("a 2M-character run estimated as %d tokens; want six figures", n)
	}
	// Natural prose keeps its estimate: words stay one token each.
	prose := strings.Repeat("the quick brown fox jumps ", 1_000)
	if n := EstimateTokens(prose); n > 10_000 {
		t.Errorf("prose over-counted: %d tokens for %d characters", n, len(prose))
	}
}

func TestTokenBudgeting(t *testing.T) {
	a := New()
	a.MaxTokens = 15
	if err := a.Add(
		Static("identity", 10, true, "You are a builder agent."),
		Static("auxiliary", 20, false, strings.Repeat("extra details for the task ", 20)),
	); err != nil {
		t.Fatal(err)
	}
	text, report := a.Assemble()
	if !strings.Contains(text, "You are a builder agent.") {
		t.Fatal("essential identity was omitted")
	}
	if strings.Contains(text, "extra details") {
		t.Fatal("auxiliary non-essential section should have been dropped for token budget")
	}
	if len(report.Omitted) == 0 {
		t.Fatal("expected report to note omitted section")
	}
}

func TestEstimateTokensCodeAndJSONCalibration(t *testing.T) {
	jsonPayload := `{"name": "devcouncil_read_file", "arguments": {"path": "main.go"}}`
	tokens := EstimateTokens(jsonPayload)
	// JSON with keys, values, quotes, and braces has ~18-25 tokens
	if tokens < 15 || tokens > 30 {
		t.Errorf("expected 15-30 tokens for JSON payload, got %d", tokens)
	}

	goFunc := "func (r *Registry) patchFile(ctx context.Context, call tools.Call) tools.Result {\n\treturn tools.Result{}\n}"
	tokens = EstimateTokens(goFunc)
	if tokens < 18 || tokens > 35 {
		t.Errorf("expected 18-35 tokens for Go function, got %d", tokens)
	}
}

func TestGuidanceRouter(t *testing.T) {
	localCfg := RouterConfig{
		Provider:     "local",
		Density:      DensityCompact,
		ActiveGroups: []string{"core", "nav"},
	}
	router := NewRouter(localCfg)
	router.Add(Static("identity-compact", 10, true, "I am local agent."), WhenDensity(DensityCompact))
	router.Add(Static("identity-full", 10, true, "I am full agent with verbose capabilities."), WhenDensity(DensityFull))
	router.Add(Static("nav-guidance", 20, false, "DevMap navigation guidance."), WhenGroupActive("nav"))
	router.Add(Static("subagent-guidance", 30, false, "Subagent fanout guidance."), WhenGroupActive("subagent"))

	sources := router.Sources()
	if len(sources) != 2 {
		t.Fatalf("expected 2 active sources for local router, got %d", len(sources))
	}
	if sources[0].Name() != "identity-compact" || sources[1].Name() != "nav-guidance" {
		t.Fatalf("unexpected active sources: %v, %v", sources[0].Name(), sources[1].Name())
	}

	text, report := router.Assemble(0)
	if !strings.Contains(text, "I am local agent.") || !strings.Contains(text, "DevMap navigation guidance.") {
		t.Fatalf("unexpected assembled text: %s", text)
	}
	if strings.Contains(text, "verbose") || strings.Contains(text, "Subagent fanout") {
		t.Fatalf("unwanted sections included in local prompt: %s", text)
	}
	if !report.Complete() {
		t.Fatalf("expected complete report, got: %+v", report)
	}
}
