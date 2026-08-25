package policy

import (
	"testing"

	"manvi/dc"
)

// The re-parse rung refuses a command word whose argument only becomes shell
// code after expansion. These cases are the two directions it can be wrong in:
// missing a spelling of `eval` that sh would still run, and firing on a line
// that merely contains the word.
func TestReparseRungFiresOnTheCommandWordAndNothingElse(t *testing.T) {
	fires := []string{
		`eval "echo hi > .env"`,
		`eval 'echo hi'`,
		`eval $CMD`,
		// Quote removal happens before sh decides what to run, so every
		// spelling that survives it invokes the same builtin.
		`"eval" "echo hi"`,
		`'eval' "echo hi"`,
		`\eval "echo hi"`,
		`'ev'al "echo hi"`,
		// Assignments precede the command word without being it.
		`FOO=1 eval "echo hi"`,
		`FOO=1 BAR=2 eval "echo hi"`,
		// Wrappers reach the builtin through another word.
		`command eval "echo hi"`,
		`builtin eval "echo hi"`,
		`command command eval "echo hi"`,
		// An assignment whose *value* is quoted is still an assignment. Reading
		// the word as a quoted literal is what a non-quote-aware splitter did
		// here, and it stopped the scan one word short of the command.
		`FOO="a b" eval "echo hi"`,
		`FOO='a b' eval "echo hi"`,
		`A=1 B="x y" C=3 eval "echo hi"`,
		// sh accepts redirections before the command word. A bare operator
		// takes the next word as its target; one with the target attached does
		// not, and stepping over the wrong number of words hid the command.
		`> out eval "echo hi"`,
		`2>/dev/null eval "echo hi"`,
		`>>log 2>&1 eval "echo hi"`,
		`< in eval "echo hi"`,
		// A later clause is its own command; the splitter judges each.
		`echo ok && eval "echo hi"`,
		`echo ok; eval "echo hi"`,
	}
	for _, command := range fires {
		d := CommandGate{HardRules: true}.EvaluateCommand(command, evalTask())
		if d.Rule != RuleCommandReparse {
			t.Errorf("%q: got rule %q, want %q", command, d.Rule, RuleCommandReparse)
			continue
		}
		if d.Severity != Hard {
			t.Errorf("%q: severity %q, want hard — a soft refusal here is demotable", command, d.Severity)
		}
	}

	// The word appears, but nothing runs it. A rung that fired on these would
	// be matching text rather than behaviour, which is the failure the
	// git-safety rules avoid by reading a dequoted variant instead of
	// substrings.
	quiet := []string{
		`echo eval`,
		`echo "eval this"`,
		`grep eval src/main.go`,
		`evaluate --report`,
		`myeval --run`,
		`git config alias.e '!eval x'`,
		`echo hi > eval`,
		`cat eval.txt`,
		// A fully quoted word is a command name, not an assignment prefix, so
		// the scan stops on it rather than stepping over it to find `eval`.
		`"FOO=1" eval`,
	}
	for _, command := range quiet {
		d := CommandGate{HardRules: true}.EvaluateCommand(command, evalTask())
		if d.Rule == RuleCommandReparse {
			t.Errorf("%q: fired the re-parse rung, but nothing here runs eval", command)
		}
	}
}

// evalTask allows every command word the cases above use, so a refusal can only
// come from the rung under test rather than from the allowlist.
func evalTask() *dc.Task {
	return &dc.Task{
		ID: "TASK-EVAL",
		AllowedCommands: []string{
			"eval *", "echo *", "grep *", "evaluate *", "myeval *", "git *",
			"cat *", "command *", "builtin *", "FOO=1 *", "FOO=1 BAR=2 *",
			`FOO="a b" *`, `FOO='a b' *`, "A=1 B=* eval *", "> out *", "2>/dev/null *",
			">>log 2>&1 *", "< in *", `"FOO=1" *`,
			`"eval" *`, `'eval' *`, `\eval *`, `'ev'al *`,
		},
	}
}
