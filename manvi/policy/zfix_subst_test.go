package policy

import (
	"strings"
	"testing"

	"manvi/dc"
)

// --- defect 2: redirections hidden inside command substitutions --------------
//
// RedirectTargets stepped over $( … ) spans wholesale. The contents of a
// substitution are recursed through policy.CommandGate.evaluate, and that
// ladder has no redirect rung of its own — the rung sits above it, in the
// caller of RedirectTargets — so a redirect inside a substitution was a write
// nothing ever judged:
//
//	echo $(git diff > ~/.ssh/authorized_keys)   allowed
//	git diff > ~/.ssh/authorized_keys           denied, hard
//
// Backticks and <( … ) happened to be caught, but only because the scanner did
// not recognise them at all and stumbled onto the `>` inside. That is not a
// property anyone can rely on: it breaks the moment the scanner learns about
// either construct. All four forms are now descended into deliberately.

func TestRedirectInsideSubstitutionIsFound(t *testing.T) {
	tests := []struct {
		command string
		targets []string
		opaque  bool
	}{
		// The reported bypass and its control must agree.
		{`echo $(git diff > ~/.ssh/authorized_keys)`, nil, true},
		{`git diff > ~/.ssh/authorized_keys`, nil, true},

		// A literal target inside a substitution is named, not swallowed.
		{`echo $(git diff > out.patch)`, []string{"out.patch"}, false},
		{"echo `git diff > out.patch`", []string{"out.patch"}, false},
		{`diff <(sort a > out.patch) b`, []string{"out.patch"}, false},
		{`tee >(gzip > out.gz) f`, []string{"out.gz"}, false},

		// Live inside double quotes too — sh executes the substitution there.
		{`echo "$(git diff > out.patch)"`, []string{"out.patch"}, false},
		{`echo "$(git diff > ~/.ssh/authorized_keys)"`, nil, true},

		// Data inside single quotes: nothing executes, nothing is written.
		{`echo '$(git diff > out.patch)'`, nil, false},

		// Nested, and combined with a redirect on the outer line.
		{`echo $(echo $(git diff > inner.patch)) > outer.patch`,
			[]string{"inner.patch", "outer.patch"}, false},

		// Arithmetic is not a substitution and carries no redirect.
		{`echo $(( 1 << 4 ))`, nil, false},

		// Unaffected existing behaviour.
		{`git diff > patch.diff`, []string{"patch.diff"}, false},
		{`echo hi`, nil, false},
	}
	for _, tc := range tests {
		targets, opaque, err := RedirectTargets(tc.command)
		if err != nil {
			t.Errorf("RedirectTargets(%q): unexpected error %v", tc.command, err)
			continue
		}
		if opaque != tc.opaque {
			t.Errorf("RedirectTargets(%q): opaque=%v, want %v", tc.command, opaque, tc.opaque)
		}
		if strings.Join(targets, "|") != strings.Join(tc.targets, "|") {
			t.Errorf("RedirectTargets(%q) = %v, want %v; a write inside a substitution "+
				"is a write nothing above this package can judge", tc.command, targets, tc.targets)
		}
	}
}

// TestSubstitutionRedirectAgreesWithTheSameRedirectAlone is the property the
// two decisions have to satisfy, stated without reference to any particular
// path: wrapping a redirect in a substitution must not change what the gate
// knows about it. A bypass is exactly a disagreement here.
func TestSubstitutionRedirectAgreesWithTheSameRedirectAlone(t *testing.T) {
	inner := []string{
		`git diff > ~/.ssh/authorized_keys`,
		`git diff > $HOME/.ssh/authorized_keys`,
		`git diff > out.patch`,
		`git diff >> out.patch`,
		`git diff 2> err.log`,
	}
	wrappers := []string{
		`echo $(%s)`,
		"echo `%s`",
		`echo "$(%s)"`,
		`diff <(%s) b`,
		`tee >(%s) f`,
		`echo $(echo $(%s))`,
	}
	for _, in := range inner {
		bare, bareOpaque, err := RedirectTargets(in)
		if err != nil {
			t.Fatalf("RedirectTargets(%q): %v", in, err)
		}
		for _, w := range wrappers {
			wrapped := strings.Replace(w, "%s", in, 1)
			got, gotOpaque, err := RedirectTargets(wrapped)
			if err != nil {
				t.Errorf("RedirectTargets(%q): unexpected error %v", wrapped, err)
				continue
			}
			if gotOpaque != bareOpaque || strings.Join(got, "|") != strings.Join(bare, "|") {
				t.Errorf("wrapping changed what the gate sees:\n  %q -> %v opaque=%v\n  %q -> %v opaque=%v",
					in, bare, bareOpaque, wrapped, got, gotOpaque)
			}
		}
	}
}

// TestSubstitutionScanFailsClosed pins the refusal direction. A substitution
// the scanner cannot bound, or one nested past the analysis limit, must produce
// an error the caller has to handle — never an empty target list, which reads
// identically to "this command writes nothing".
func TestSubstitutionScanFailsClosed(t *testing.T) {
	for _, cmd := range []string{
		`echo $(git diff > out.patch`,
		"echo `git diff > out.patch",
		`echo $(echo $(git diff > out.patch)`,
		`diff <(sort a b`,
	} {
		if _, _, err := RedirectTargets(cmd); err == nil {
			t.Errorf("RedirectTargets(%q) returned no error; an unbounded span must refuse", cmd)
		}
	}

	// Nesting past the limit must refuse rather than recurse forever or return
	// a silently short list.
	deep := "git diff > out.patch"
	for i := 0; i <= maxSubstitutionDepth+1; i++ {
		deep = "echo $(" + deep + ")"
	}
	if _, _, err := RedirectTargets(deep); err == nil {
		t.Error("RedirectTargets returned no error for a substitution nested past the analysis limit")
	}
}

// --- defect 3: normalisation laundering a path that is not this repo's CLI ---
//
// NormalizeAllowlistCommand rewrote any token whose basename folded to "dev"
// under a parent folding to "bin" or "scripts". Matching then ran on the
// rewritten string while sh ran the original, so an attacker-chosen binary
// inherited every `dev …` allowlist entry.

func TestNormalizationDoesNotLaunderForeignPaths(t *testing.T) {
	// Each of these once normalised to a bare "dev" and was allowed.
	launderers := []string{
		"/tmp/attacker/bin/dev status",
		// An absolute path at the virtualenv layout is the same laundering in
		// a more convincing costume: /tmp/attacker/.venv/bin/dev is shaped
		// exactly like a project venv install, and only containment in the
		// working tree tells the two apart. This entry point has no tree to
		// compare against, so it attributes no absolute path to one. The
		// rooted form is NormalizeAllowlistCommandInRoot, and it still accepts
		// the repository's own — see TestAnAbsoluteVenvDevInsideTheRootIsStillThisReposCLI.
		"/abs/path/.venv/bin/dev status",
		"../../../../tmp/attacker/bin/dev status",
		"attacker/scripts/dev run-cmd anything",
		"/tmp/x/bin/DEV status",
		"/tmp/x/BIN/dev status",
		`attacker\bin\dev status`,
		`..\..\tmp\attacker\bin\dev status`,
		"/tmp/attacker/scripts/devcouncil status",
		"a/b/bin/dev status",
		"./../bin/dev status",
		"/bin/dev status",
		"/tmp/attacker/bin/DevCouncil status",
	}
	gate := CommandGate{HardRules: true}
	for _, cmd := range launderers {
		if got := NormalizeAllowlistCommand(cmd); got != collapseSpaces(cmd) {
			t.Errorf("NormalizeAllowlistCommand(%q) = %q; a foreign path was rewritten "+
				"into this repo's dev CLI", cmd, got)
		}
		if d := gate.EvaluateCommand(cmd, nil); d.Action != Deny {
			t.Errorf("EvaluateCommand(%q, nil) = %v (%s); the allowlist was reached through "+
				"a laundered path", cmd, d.Action, d.Reason)
		}
	}
}

// TestNormalizationStillAcceptsThisReposDevCLI is the other half: the fix must
// not simply stop normalising, or every hook-installed `.venv/bin/dev` breaks.
// These are the forms the parity fixture pins as this repo's own.
func TestNormalizationStillAcceptsThisReposDevCLI(t *testing.T) {
	accepted := map[string]string{
		"dev status":                 "dev status",
		"devcouncil status":          "dev status",
		".venv/bin/dev map":          "dev map",
		"./scripts/dev map":          "dev map",
		"scripts/dev map":            "dev map",
		"bin/dev map":                "dev map",
		"venv/bin/dev map":           "dev map",
		".venv/Scripts/dev.exe map":  "dev map",
		"uv run --project . dev map": "uv run dev map",
		"DevCouncil/dev map":         "DevCouncil/dev map",
	}
	for cmd, want := range accepted {
		if got := NormalizeAllowlistCommand(cmd); got != want {
			t.Errorf("NormalizeAllowlistCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// TestNormalizationCannotBeReachedThroughAnEscapedChain ties the two fixed
// rungs together. The splitter now judges each part on its own, so a laundered
// dev path chained behind an escape gets its own verdict rather than riding on
// the allowlisted command in front of it.
func TestNormalizationCannotBeReachedThroughAnEscapedChain(t *testing.T) {
	gate := CommandGate{HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	for _, cmd := range []string{
		`echo \' ; /tmp/attacker/bin/dev status`,
		`echo $'\'' && attacker/scripts/dev run-cmd anything`,
		`echo ok | /tmp/x/bin/DEV status`,
	} {
		if d := gate.EvaluateCommand(cmd, task); d.Action != Deny {
			t.Errorf("EvaluateCommand(%q) = %v (%s), want deny", cmd, d.Action, d.Reason)
		}
	}
}
