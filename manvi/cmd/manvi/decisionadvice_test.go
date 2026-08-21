package main

import (
	"bytes"
	"strings"
	"testing"

	"manvi/policy"
)

// TestPrintedOverrideAdviceIsRunnable guards the operator half of the recovery
// invariant: what `manvi check` prints has to be a command that grants what was
// refused when it is pasted back into a shell.
//
// It did not. A command block printed `manvi allow grep -rn package src
// --reason "..."`, and running that issued a human grant — eight-hour ceiling —
// for a *file* named "grep", while the command stayed blocked.
func TestPrintedOverrideAdviceIsRunnable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision policy.Decision
		want     string
	}{
		{
			name: "command block names the command flag",
			decision: policy.Decision{
				Action: policy.Deny, Rule: policy.RuleCommandNotAllowed,
				Severity: policy.Soft, Target: "grep -rn package src",
			},
			want: `manvi allow --cmd 'grep -rn package src' --reason`,
		},
		{
			name: "path block names no flag",
			decision: policy.Decision{
				Action: policy.Deny, Rule: policy.RuleUnplannedScope,
				Severity: policy.Soft, Target: "src/calc.go",
			},
			want: `manvi allow src/calc.go --reason`,
		},
		{
			name: "a path containing a space is quoted",
			decision: policy.Decision{
				Action: policy.Deny, Rule: policy.RuleUnplannedScope,
				Severity: policy.Soft, Target: "my dir/a.go",
			},
			want: `manvi allow 'my dir/a.go' --reason`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printDecision(&out, tc.decision)
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("advice did not contain %q:\n%s", tc.want, out.String())
			}
		})
	}
}

// TestShellQuoteSurvivesAShell checks the quoting itself against the characters
// that change a command's meaning rather than its text.
func TestShellQuoteSurvivesAShell(t *testing.T) {
	for _, raw := range []string{
		"plain.go", "with space.go", "it's.go", "a;rm -rf b", "$HOME/x", "`id`",
		"a\"b", "a|b", "a&b", "*", "", "a\nb",
	} {
		quoted := shellQuote(raw)
		if raw != "" && quoted == raw && strings.ContainsAny(raw, " ;|&$`\"'*\n") {
			t.Errorf("shellQuote(%q) = %q left a metacharacter unquoted", raw, quoted)
		}
		if got := unquoteSingle(quoted); got != raw {
			t.Errorf("shellQuote(%q) = %q, which a shell reads back as %q", raw, quoted, got)
		}
	}
}

// unquoteSingle applies POSIX single-quote rules to one fully-quoted word, the
// way a shell would.
func unquoteSingle(s string) string {
	if !strings.HasPrefix(s, "'") {
		return s
	}
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inQuote = !inQuote
		case s[i] == '\\' && !inQuote && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
