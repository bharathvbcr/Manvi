package prompt

import (
	"fmt"
	"strings"
	"testing"
)

// The budget must never be able to drop a rule.
//
// This is the property Essential exists for, and it is a safety property rather
// than a quality one: a model told it may write anywhere, because the policy
// section did not fit, is a model that will try. Asserting it at a couple of
// hand-picked budgets proves nothing about the budget one release from now, so
// it is asserted across the whole range including the degenerate ends.
func TestNoBudgetEverDropsAnEssentialSection(t *testing.T) {
	essential := []string{"identity", "posture", "policy", "tasks", "tool-contract"}
	optional := []string{"working-method", "project-instructions", "nav-guidance"}

	build := func() *Assembler {
		a := New()
		for i, name := range essential {
			_ = a.Add(Static(name, 10*(i+1), true,
				name+": "+strings.Repeat("rule text ", 20)))
		}
		for i, name := range optional {
			_ = a.Add(Static(name, 500+i, false,
				name+": "+strings.Repeat("guidance ", 200)))
		}
		return a
	}

	for _, budget := range []int{-1, 0, 1, 2, 7, 13, 50, 100, 250, 500, 1000, 5000, 1 << 20} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			a := build()
			a.MaxTokens = budget
			text, report := a.Assemble()

			for _, name := range essential {
				if !strings.Contains(text, name+":") {
					t.Errorf("budget %d dropped essential section %q", budget, name)
				}
			}
			// Every section must be accounted for either way: a section that
			// is in neither list is one the report cannot explain.
			seen := map[string]bool{}
			for _, in := range report.Included {
				seen[in.Name] = true
			}
			for _, out := range report.Omitted {
				seen[out.Name] = true
			}
			for _, name := range append(append([]string{}, essential...), optional...) {
				if !seen[name] {
					t.Errorf("budget %d: section %q appears in neither Included nor Omitted", budget, name)
				}
			}
		})
	}
}

// A rune budget must behave the same way. Two budgets that can each drop a
// section independently is two chances to drop a rule.
func TestNoRuneBudgetEverDropsAnEssentialSection(t *testing.T) {
	for _, budget := range []int{-1, 0, 1, 5, 40, 200, 1 << 20} {
		a := New()
		a.MaxRunes = budget
		_ = a.Add(Static("policy", 10, true, "a blocked write is not a reason to try another path"))
		_ = a.Add(Static("guidance", 20, false, strings.Repeat("advice ", 500)))
		text, _ := a.Assemble()
		if !strings.Contains(text, "blocked write") {
			t.Errorf("rune budget %d dropped the policy section", budget)
		}
	}
}

// Assembly must be deterministic: the prompt is diffed across runs and cached
// by providers, and bytes that move for no reason defeat both.
func TestAssemblyIsByteIdenticalAcrossRuns(t *testing.T) {
	build := func() *Assembler {
		a := New()
		a.MaxTokens = 300
		// Registered out of order and with colliding Order values, so the tie
		// break by name is what has to be doing the work.
		_ = a.Add(Static("zeta", 10, true, "z"))
		_ = a.Add(Static("alpha", 10, true, "a"))
		_ = a.Add(Static("mid", 5, false, "m"))
		_ = a.Add(Static("omega", 10, false, strings.Repeat("o ", 400)))
		return a
	}
	first, firstReport := build().Assemble()
	for i := 0; i < 50; i++ {
		got, report := build().Assemble()
		if got != first {
			t.Fatalf("run %d produced different bytes:\n%q\nvs\n%q", i, got, first)
		}
		if len(report.Included) != len(firstReport.Included) {
			t.Fatalf("run %d included a different number of sections", i)
		}
	}
}

// A contributor that fails must not take the run down, and must not be silent.
func TestAFailingSourceIsReportedAndTheRestStillAssemble(t *testing.T) {
	a := New()
	_ = a.Add(SourceFunc{Label: "broken", Fn: func() ([]Section, error) {
		return nil, fmt.Errorf("open /nope: no such file")
	}})
	_ = a.Add(Static("policy", 10, true, "the rule"))

	text, report := a.Assemble()
	if !strings.Contains(text, "the rule") {
		t.Error("a failing contributor took down the sections that did load")
	}
	if len(report.Failed) != 1 {
		t.Fatalf("Failed = %v, want the broken contributor named", report.Failed)
	}
	if report.Complete() {
		t.Error("a prompt assembled while a contributor was failing reported itself complete")
	}
}

// Adversarial section content must not corrupt assembly.
func TestAdversarialSectionContentIsAssembledIntact(t *testing.T) {
	nasty := map[string]string{
		"nul":        "before\x00after",
		"newlines":   strings.Repeat("\n", 500) + "content" + strings.Repeat("\n", 500),
		"unicode":    "日本語 🔥 ‮ reversed",
		"whitespace": "   \t\t   content   \t  ",
		"huge":       strings.Repeat("x", 200_000),
	}
	for name, body := range nasty {
		t.Run(name, func(t *testing.T) {
			a := New()
			_ = a.Add(Static(name, 10, true, body))
			text, report := a.Assemble()
			if len(report.Included) != 1 {
				t.Fatalf("section %q was not included: %+v", name, report)
			}
			if report.Runes != len([]rune(text)) {
				t.Errorf("Runes = %d but the text has %d", report.Runes, len([]rune(text)))
			}
			if strings.TrimSpace(body) != "" && text == "" {
				t.Error("non-empty content assembled to nothing")
			}
		})
	}
}

// EstimateTokens backs every budget decision, so its failure modes are the
// budget's failure modes.
func TestEstimateTokensIsSaneAndNeverNegative(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
	}
	for _, s := range []string{"a", " ", "\n", "\x00", "日", strings.Repeat("word ", 1000)} {
		if got := EstimateTokens(s); got < 0 {
			t.Errorf("EstimateTokens(%q) = %d, want >= 0", s, got)
		}
	}
	// Any non-empty input must cost at least one token, or a budget could be
	// spent without ever decrementing.
	for _, s := range []string{"a", " ", "\x00", "日"} {
		if got := EstimateTokens(s); got < 1 {
			t.Errorf("EstimateTokens(%q) = %d, want at least 1", s, got)
		}
	}
	// Growing the input must never shrink the estimate.
	prev := 0
	acc := ""
	for i := 0; i < 200; i++ {
		acc += "token "
		got := EstimateTokens(acc)
		if got < prev {
			t.Fatalf("estimate shrank as input grew: %d -> %d at step %d", prev, got, i)
		}
		prev = got
	}
}

// FuzzAssembleNeverDropsAnEssentialSection drives the same safety property with
// arbitrary content and budgets.
func FuzzAssembleNeverDropsAnEssentialSection(f *testing.F) {
	f.Add("policy text", "guidance text", 0)
	f.Add("", strings.Repeat("g", 5000), 3)
	f.Add("\x00\n\t", "日本語", -7)

	f.Fuzz(func(t *testing.T, rule, guidance string, budget int) {
		a := New()
		a.MaxTokens = budget
		_ = a.Add(Static("policy", 10, true, rule))
		_ = a.Add(Static("guidance", 20, false, guidance))
		text, report := a.Assemble()

		// An empty section contributes nothing by design and is reported
		// omitted, so the property only binds when there was content.
		if strings.TrimSpace(rule) == "" {
			return
		}
		if !strings.Contains(text, strings.TrimSpace(rule)) {
			t.Fatalf("budget %d dropped the essential section (rule=%q)", budget, rule)
		}
		if report.Runes != len([]rune(text)) {
			t.Fatalf("Runes = %d but text has %d", report.Runes, len([]rune(text)))
		}
	})
}
