package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Attacks on the exit-status contract of runGitCapture and on the verdict
// settle. Every shim below is a git stand-in that misbehaves in one way and
// hands every other call to the real git (see shimGit).

const zadvPartialDiff = "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+partial"

// zadvOnNoIndex runs body only for a --no-index call.
func zadvOnNoIndex(body string) string {
	return "case \" $* \" in *\" --no-index \"*)\n" + body + "\n;; esac"
}

func TestZadv_w24va_NonNoIndexExit1WithPartialOutputIsRefused(t *testing.T) {
	root := newGitRepo(t)
	shimGit(t, "case \" $* \" in *\" --no-index \"*) ;; *\" diff \"*)\nprintf '"+
		strings.ReplaceAll(zadvPartialDiff, "\n", "\\n")+"'\nexit 1\n;; esac")

	if diff, _, err := gitDiff(context.Background(), root); err == nil {
		t.Fatalf("gitDiff accepted exit 1 with partial output from a plain diff:\n%s", diff)
	}
	if c, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitExitOK, "diff", "HEAD"); err == nil {
		t.Fatalf("runGitCapture{0} accepted exit 1: %q", c.out)
	}
}

func TestZadv_w24va_NoIndexExit1WithEmptyOutputIsAFailureNotAnEmptyDiff(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "new.txt", "x\n")
	shimGit(t, zadvOnNoIndex("echo 'error: cannot render' >&2\nexit 1"))

	renders, _ := renderUntracked(context.Background(), root, []string{"new.txt"}, maxGitCaptureBytes)
	if len(renders) != 1 || renders[0].err == nil {
		t.Fatalf("an empty --no-index exit 1 was taken for a diff: %+v", renders)
	}
	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if strings.Contains(diff, "new.txt") || !strings.Contains(strings.Join(notes, "\n"), "new.txt") {
		t.Fatalf("a file git could not render is neither in the diff nor named in the notes: notes=%q", notes)
	}
}

func TestZadv_w24va_NoIndexOtherExitCodesWithPartialOutputAreRefused(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "new.txt", "x\n")
	for _, code := range []string{"2", "128", "129", "255"} {
		shimGit(t, zadvOnNoIndex("printf '"+strings.ReplaceAll(zadvPartialDiff, "\n", "\\n")+"'\nexit "+code))
		c, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitDiffNoIndexExit, untrackedDiffArgs("new.txt")...)
		if err == nil {
			t.Errorf("exit %s with partial output was accepted: %q", code, c.out)
		}
	}
}

func TestZadv_w24va_AGitKilledBySignalIsRefusedWhateverItWrote(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "new.txt", "x\n")
	for _, sig := range []string{"-9", "-PIPE", "-TERM", "-SEGV"} {
		shimGit(t, zadvOnNoIndex("printf '"+strings.ReplaceAll(zadvPartialDiff, "\n", "\\n")+"'\nkill "+sig+" $$"))
		c, err := runGitCapture(context.Background(), root, maxGitCaptureBytes, gitDiffNoIndexExit, untrackedDiffArgs("new.txt")...)
		if err == nil {
			t.Errorf("a git killed by %s was accepted: %q", sig, c.out)
		}
	}
	shimGit(t, "case \" $* \" in *\" --no-index \"*) ;; *\" diff \"*)\nprintf 'diff --git a/s b/s\\n'\nkill -9 $$\n;; esac")
	if diff, _, err := gitDiff(context.Background(), root); err == nil {
		t.Fatalf("gitDiff accepted a diff from a git killed by SIGKILL:\n%s", diff)
	}
}

func TestZadv_w24va_TheNoIndexPathIsStillCapped(t *testing.T) {
	root := newGitRepo(t)
	var big strings.Builder
	for big.Len() < 200<<10 {
		big.WriteString("a line long enough to make this file worth cutting\n")
	}
	write(t, root, "big.txt", big.String())

	for _, cap := range []int{0, 64, 4000} { // 0: the default speculative cap; the rest force the re-render
		if cap > 0 {
			withSpeculativeCap(t, cap)
		}
		renders, _ := renderUntracked(context.Background(), root, []string{"big.txt"}, 5000)
		if len(renders) != 1 || renders[0].err != nil {
			t.Fatalf("cap %d: %+v", cap, renders)
		}
		if len(renders[0].added) > 5000 || renders[0].note == "" {
			t.Errorf("cap %d: an oversized untracked file was not refused at the budget: %d bytes, note %q",
				cap, len(renders[0].added), renders[0].note)
		}
	}
}

func TestZadv_w24va_AnOversizedUntrackedFileIsCutAndSaidSoThroughGitDiff(t *testing.T) {
	root := newGitRepo(t)
	line := strings.Repeat("x", 78) + "\n"
	write(t, root, "huge.txt", strings.Repeat(line, (maxGitCaptureBytes+(1<<20))/len(line)))

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if len(diff) > maxGitCaptureBytes {
		t.Errorf("diff is %d bytes, over the %d cap", len(diff), maxGitCaptureBytes)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "huge.txt") {
		t.Errorf("a file cut at the cap is not named in the notes: %q", notes)
	}
}

func TestZadv_w24va_ANestedRepoOrDirectoryIsNeverSilentlySkipped(t *testing.T) {
	root := newGitRepo(t)
	nested := filepath.Join(root, "inner")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	run(t, nested, "init", "-q")
	run(t, nested, "config", "user.email", "n@test")
	run(t, nested, "config", "user.name", "n")
	write(t, nested, "f.go", "package inner\n")
	run(t, nested, "add", ".")
	run(t, nested, "commit", "-qm", "n")
	write(t, root, "plain/dir/g.go", "package g\n")

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if !strings.Contains(diff, "inner") && !strings.Contains(strings.Join(notes, "\n"), "inner") {
		t.Errorf("the nested repository left no trace in the diff or the notes: notes=%q", notes)
	}
	if !strings.Contains(diff, "plain/dir/g.go") {
		t.Errorf("a file under an untracked directory is not in the diff")
	}
	// A directory handed to the renderer directly.
	for _, p := range []string{"plain", "plain/dir", "inner"} {
		renders, _ := renderUntracked(context.Background(), root, []string{p}, maxGitCaptureBytes)
		if len(renders) != 1 {
			t.Fatalf("%s: %+v", p, renders)
		}
		if u := renders[0]; u.err == nil && u.added == "" && u.note == "" {
			t.Errorf("%s: rendered as nothing, with no error and no note", p)
		}
	}
}

func TestZadv_w24va_ATimedOutGitIsAnErrorNotAnEmptyDiff(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "new.txt", "x\n")
	shimGit(t, zadvOnNoIndex("sleep 20"))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if c, err := runGitCapture(ctx, root, maxGitCaptureBytes, gitDiffNoIndexExit, untrackedDiffArgs("new.txt")...); err == nil {
		t.Fatalf("a timed-out git was accepted: %q", c.out)
	}
	renders, _ := renderUntracked(ctx, root, []string{"new.txt"}, maxGitCaptureBytes)
	if len(renders) != 1 || renders[0].err == nil {
		t.Fatalf("a timed-out render was taken for a diff: %+v", renders)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the timeout was not honoured: %v", time.Since(start))
	}
}

// The three call sites changed their tolerance from "any non-zero with output"
// to "only listed codes". A caller that reads a partial listing as whole is the
// silent skip: the file never reaches the secret scan.
func TestZadv_w24va_APartialUntrackedListingIsNamedNotTrustedAsWhole(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "a.go", "package a\n")
	write(t, root, "b.go", "package b\n")
	shimGit(t, "case \" $* \" in *\" ls-files \"*)\nprintf 'a.go\\0'\necho 'fatal: died' >&2\nexit 1\n;; esac")

	_, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "untracked-file listing failed") {
		t.Fatalf("a partial listing from a failed git was used without a note: %q", notes)
	}
}

func TestZadv_w24va_AScopedDiffFromAFailedGitDegradesTheVerdict(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "seed.txt", "changed\n")
	shimGit(t, "case \" $* \" in *\" --no-index \"*) ;; *\" diff \"*)\nprintf '"+
		strings.ReplaceAll(zadvPartialDiff, "\n", "\\n")+"'\nexit 1\n;; esac")
	v, err := NewPathVerifier(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := v.VerifyPaths(context.Background(), []string{"seed.txt"}, "", Baseline{})
	if got.Verdict == VerdictPassed || !strings.Contains(strings.Join(got.Degraded, "\n"), "scoped diff could not be produced") {
		t.Fatalf("a failed scoped diff read as usable: %+v", got)
	}
}

func TestZadv_w24va_NoVerifierIsDegradedInBothVerifiersAndCoverageNoteAloneIsNot(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	write(t, f.root, "src/calc.go", "package calc\n")
	run(t, f.root, "add", "src/calc.go")
	run(t, f.root, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "calc")
	write(t, f.root, "src/calc.go", "package calc\n\nconst X = 1\n")

	// Verifier present, coverage unmeasured: the one exemption.
	f.reg.deps.CoverageFile = ""
	report, err := f.reg.report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictPassed || !report.Passed {
		t.Fatalf("coverage-unmeasured alone: %q passed=%v degraded=%v gaps=%v", report.Verdict, report.Passed, report.Degraded, report.Gaps)
	}
	paths := f.reg.VerifyPaths(t.Context(), []string{"src/calc.go"}, "", Baseline{})
	if paths.Verdict != report.Verdict {
		t.Errorf("the two verifiers disagree on the same state: report=%q VerifyPaths=%q (%v)", report.Verdict, paths.Verdict, paths.Degraded)
	}

	// The exemption must not hide a real degradation riding with the note: a
	// capped untracked listing in the same run.
	shimGit(t, "case \" $* \" in *\" ls-files \"*)\necho 'fatal: cannot list' >&2\nexit 128\n;; esac")
	report, err = f.reg.report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Verdict != VerdictDegraded {
		t.Fatalf("a real degradation beside the coverage note settled as %q passed=%v: %v", report.Verdict, report.Passed, report.Degraded)
	}

	// No verifier at all, in both.
	f.reg.deps.VerifierBinary = ""
	paths = f.reg.VerifyPaths(t.Context(), []string{"src/calc.go"}, "", Baseline{})
	if paths.Verdict != VerdictDegraded {
		t.Errorf("VerifyPaths without a verifier = %q", paths.Verdict)
	}
}

// degraded is a list in every projection of a clean run, never absent and never
// null: a reader cannot take a missing field for "nothing degraded".
func TestZadv_w24va_EveryVerificationToolCarriesDegradedAsAnEmptyListWhenClean(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}
	for _, name := range []string{"devcouncil_verify_task", "devcouncil_get_gaps", "devcouncil_get_next_actions"} {
		body := f.payload(name, map[string]any{})
		if body["verdict"] != VerdictPassed {
			t.Fatalf("%s: setup is not a clean run: %v", name, body)
		}
		list, ok := body["degraded"].([]any)
		if !ok || list == nil || len(list) != 0 {
			t.Errorf("%s: degraded = %#v (present=%v), want []", name, body["degraded"], ok)
		}
	}
}
