package devcouncil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/fnmatch"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

const fakeToken = "FAKE-SUPERSECRET-0000"

// requireSeatbelt skips, with the reason, where the live checks cannot run:
// off macOS, without sandbox-exec, or when this process is itself inside a
// sandbox that forbids nesting one. A skip is a check that did not run, and
// the message says so.
func requireSeatbelt(t *testing.T) {
	t.Helper()
	launcher, err := containmentLookup()
	if err != nil {
		t.Skipf("live containment check did not run: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, code, timedOut, err := runArgv(ctx, t.TempDir(), []string{launcher, "-p", "(version 1)(allow default)", "true"})
	if err != nil || timedOut || code != 0 {
		t.Skipf("live containment check did not run: sandbox-exec cannot start here (exit %d, err %v): %s", code, err, out)
	}
}

func writeTree(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runContained(t *testing.T, root, command string) (string, int) {
	t.Helper()
	argv, err := containArgv(context.Background(), root, nil, []string{"sh", "-c", command})
	if err != nil {
		t.Fatalf("containArgv: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, code, timedOut, err := runArgv(ctx, root, argv)
	if err != nil || timedOut {
		t.Fatalf("contained %q: err %v timedOut %v: %s", command, err, timedOut, out)
	}
	return out, code
}

func TestSBPLStringEscapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/a/b", `"/a/b"`},
		{`/a/we"ird`, `"/a/we\"ird"`},
		{`/a/back\slash`, `"/a/back\\slash"`},
		{`/a/\"`, `"/a/\\\""`},
		{"/a/line\nbreak\ttab\r", `"/a/line\nbreak\ttab\r"`},
		{"/a/ключ-ünï-鍵", `"/a/ключ-ünï-鍵"`},
	} {
		got, err := sbplString(c.in)
		if err != nil || got != c.want {
			t.Errorf("sbplString(%q) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"/a/\xff", "/a/\x00b", "/a/\x1bb", "/a/\x7f"} {
		if got, err := sbplString(bad); err == nil {
			t.Errorf("sbplString(%q) = %s, want an error: it has no safe literal", bad, got)
		}
	}
}

// profileRule is one deny entry read back out of a rendered profile.
var profileRule = regexp.MustCompile(`\((literal|subpath) "((?:[^"\\]|\\.)*)"\)`)

func unescapeSBPL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// readDenyBlock is the file-read* deny form of a rendered profile: the rules
// that name secret files, apart from the write, link and unlink forms that
// protect them from being moved.
func readDenyBlock(t *testing.T, profile string) string {
	t.Helper()
	_, rest, ok := strings.Cut(profile, "(deny file-read*")
	if !ok {
		t.Fatalf("the profile has no file-read* deny:\n%s", profile)
	}
	block, _, _ := strings.Cut(rest, "\n(deny ")
	return block
}

// The profile must deny exactly the files the policy matcher calls secret, no
// more and no fewer, over a tree built from DevCouncil's own fold fixture (the
// rows its Go and Rust engines are held to) plus ordinary files.
func TestProfileDeniesExactlyWhatTheMatcherMatches(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	fixture := filepath.Join(filepath.Dir(self), "..", "..", "..", "DevCouncil", "testdata", "secret-paths-fold.tsv")
	raw, err := os.ReadFile(fixture) // #nosec G304 -- the pinned DevCouncil checkout's fixture
	if err != nil {
		t.Fatalf("the DevCouncil fold fixture is the parity oracle and is missing: %v", err)
	}
	var rows []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t")[0])
	}
	rows = append(rows, "README.md", "src/lib/util.go", ".git/config", "docs/secret.md", "keys/readme.txt",
		"app/.aws/credentials", ".envrc", "deep/er/id_ed25519", "certs/ca.p12", "pkg/credentials/token", ".github/workflows/ci.yml")

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		path := filepath.Join(root, filepath.FromSlash(row))
		if _, err := os.Lstat(path); err == nil { // #nosec G703 -- a path under this test's own t.TempDir
			continue // a case-insensitive filesystem already holds this spelling
		}
		writeTree(t, root, map[string]string{row: "x"})
	}

	// Expected: the matcher over what is on disk, read by a walk of this test's own.
	want := map[string]bool{}
	var all []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		all = append(all, p)
		if fnmatch.MatchAnyFold(policy.SecretPathPatterns, filepath.ToSlash(rel)) {
			want[p] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	denials, err := findSecretDenials(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile(denials)
	if err != nil {
		t.Fatal(err)
	}
	var literals, subpaths []string
	for _, m := range profileRule.FindAllStringSubmatch(readDenyBlock(t, profile), -1) {
		if m[1] == "literal" {
			literals = append(literals, unescapeSBPL(m[2]))
		} else {
			subpaths = append(subpaths, unescapeSBPL(m[2]))
		}
	}
	got := map[string]bool{}
	for _, p := range all {
		for _, l := range literals {
			if p == l {
				got[p] = true
			}
		}
		for _, s := range subpaths {
			if strings.HasPrefix(p, s+"/") {
				got[p] = true
			}
		}
	}
	var missing, extra []string
	for p := range want {
		if !got[p] {
			missing = append(missing, p)
		}
	}
	for p := range got {
		if !want[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		t.Fatalf("profile and matcher disagree\nmatched but not denied: %q\ndenied but not matched: %q\nprofile:\n%s", missing, extra, profile)
	}
	if len(want) < 15 || len(all)-len(want) < 5 {
		t.Fatalf("the fixture did not exercise both sides: %d secret of %d files", len(want), len(all))
	}
	// Every rule in the profile names a path under the resolved root.
	for _, p := range append(literals, subpaths...) {
		if !strings.HasPrefix(p, root+"/") {
			t.Errorf("rule %q is outside the resolved root %q", p, root)
		}
	}
}

// Quotes, backslashes, control whitespace and non-ASCII in a secret path must
// reach seatbelt as the file they name.
func TestOddlyNamedSecretFilesAreDeniedLive(t *testing.T) {
	requireSeatbelt(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	odd := []string{`we"ird.key`, `back\slash.pem`, "ключ-ünï.key", "tab\there.key", "new\nline.pem", `a"b\c"\.key`}
	files := map[string]string{"ok.txt": "ORDINARY"}
	for _, name := range odd {
		files["dir/"+name] = fakeToken
	}
	writeTree(t, root, files)

	script := "for f in dir/*; do if cat \"$f\" >/dev/null 2>&1; then echo \"READ:$f\"; fi; done; cat ok.txt"
	out, code := runContained(t, root, script)
	if strings.Contains(out, "READ:") {
		t.Fatalf("a secret-named file was readable under containment:\n%s", out)
	}
	if code != 0 || !strings.Contains(out, "ORDINARY") {
		t.Fatalf("the ordinary file was not readable (exit %d): %s", code, out)
	}
	// And without containment each of them is readable, so the above could fail.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	open, _, _, err := runShell(ctx, root, script)
	if err != nil || strings.Count(open, "READ:") != len(odd) {
		t.Fatalf("uncontained, %d of %d odd files read (err %v):\n%s", strings.Count(open, "READ:"), len(odd), err, open)
	}
}

// The finding this seam exists for: grep -r and a program the agent runs read a
// secret file that a command naming it would have been refused.
func TestContainmentStopsRecursiveReadersLive(t *testing.T) {
	requireSeatbelt(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		"secrets/vault.go": "package secrets\nconst Token = \"" + fakeToken + "\"\n",
		".env":             "TOKEN=" + fakeToken + "\n",
		"main.go":          "package main // MAIN-MARKER\n",
	})
	if err := os.Symlink("secrets/vault.go", filepath.Join(root, "innocent.txt")); err != nil {
		t.Fatal(err)
	}
	readers := map[string]string{
		"grep -r":    "grep -r SUPERSECRET .",
		"perl open":  `perl -e 'open(F, "secrets/vault.go") or exit 3; print <F>'`,
		"perl .env":  `perl -e 'open(F, ".env") or exit 3; print <F>'`,
		"via a link": "perl -e 'open(F, \"innocent.txt\") or exit 3; print <F>'",
	}

	// Uncontained, as shipped: every one of them leaks.
	for name, command := range readers {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _, _, err := runShell(ctx, root, command)
		cancel()
		if err != nil || !strings.Contains(out, fakeToken) {
			t.Errorf("uncontained %s did not read the file (the documented behaviour): %v %q", name, err, out)
		}
	}
	// Contained: none does, and ordinary reads work.
	for name, command := range readers {
		out, code := runContained(t, root, command)
		if strings.Contains(out, fakeToken) {
			t.Errorf("contained %s leaked the secret: %q", name, out)
		}
		if code == 0 && name != "grep -r" {
			t.Errorf("contained %s exited 0 having read nothing: %q", name, out)
		}
	}
	out, code := runContained(t, root, "grep -r MAIN-MARKER . ; cat main.go")
	if code != 0 || strings.Count(out, "MAIN-MARKER") < 2 {
		t.Fatalf("ordinary reads failed under containment (exit %d): %s", code, out)
	}
	// A file created in a secret directory while the command runs is covered,
	// because the directory is denied as a subtree rather than file by file.
	out, _ = runContained(t, root, "echo "+fakeToken+" > secrets/new.txt; cat secrets/new.txt")
	if strings.Contains(out, fakeToken) {
		t.Errorf("a file created in a secret directory mid-command was readable: %q", out)
	}
}

// With sandbox-exec unavailable, on is a refusal, and the command does not run.
func TestContainmentFailsClosedWhenUnavailable(t *testing.T) {
	old := containmentLookup
	t.Cleanup(func() { containmentLookup = old })
	containmentLookup = func() (string, error) {
		return "", errors.New("sandbox-exec exists only on macOS and this host is plan9")
	}

	if argv, err := containArgv(context.Background(), t.TempDir(), nil, []string{"sh", "-c", "true"}); err == nil || argv != nil {
		t.Fatalf("containArgv = %v, %v; want no argv and an error", argv, err)
	}

	f := newFixture(t)
	f.reg.deps.ExecContainment = ContainmentSecrets
	res := f.call("devcouncil_exec_command", map[string]string{"command": "git status"})
	if !res.IsError || !strings.Contains(res.Text, "containment") || !strings.Contains(res.Text, "unavailable") ||
		!strings.Contains(res.Text, "not run") || strings.Contains(res.Text, "exit code") {
		t.Fatalf("on without a launcher must refuse and say so, got: %s", res.Text)
	}

	// Off is untouched by the missing launcher.
	f.reg.deps.ExecContainment = ContainmentOff
	if res := f.call("devcouncil_exec_command", map[string]string{"command": "git status"}); res.IsError {
		t.Fatalf("off must not depend on a launcher: %s", res.Text)
	}
	f.reg.deps.ExecContainment = "secret"
	if res := f.call("devcouncil_exec_command", map[string]string{"command": "git status"}); !res.IsError ||
		!strings.Contains(res.Text, "unknown value") {
		t.Fatalf("a misspelt mode must refuse, got: %s", res.Text)
	}
}

// Through the registry, with the real launcher: a command runs, contained.
func TestExecCommandRunsContainedLive(t *testing.T) {
	requireSeatbelt(t)
	// Dev posture, where the command allowlist does not refuse grep: the leak
	// the design document reproduced is only reachable there.
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	f.reg.deps.ExecContainment = ContainmentOff
	if err := os.WriteFile(filepath.Join(f.root, "vault.txt"), []byte("T="+fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.root, "vault.txt"), filepath.Join(f.root, ".env")); err != nil {
		t.Fatal(err)
	}
	if res := f.call("devcouncil_exec_command", map[string]string{"command": "grep -r SUPERSECRET ."}); !strings.Contains(res.Text, fakeToken) {
		t.Fatalf("uncontained, grep -r should read the file (the documented behaviour): %s", res.Text)
	}
	f.reg.deps.ExecContainment = ContainmentSecrets
	if err := os.WriteFile(filepath.Join(f.root, ".env"), []byte("T="+fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call("devcouncil_exec_command", map[string]string{"command": "git status"})
	if res.IsError || !strings.Contains(res.Text, "exit code 0") {
		t.Fatalf("git status under containment: %s", res.Text)
	}
	res = f.call("devcouncil_exec_command", map[string]string{"command": "grep -r SUPERSECRET ."})
	if strings.Contains(res.Text, fakeToken) {
		t.Fatalf("grep -r leaked through the registry under containment: %s", res.Text)
	}
}

// A read deny alone is bypassed by moving the secret out from under its name:
// rename the file, rename a directory above it, or hard-link it. Each of those
// must fail under containment and leave the token unread, while an ordinary
// write beside them still works.
func TestContainmentStopsRenameAndLinkLive(t *testing.T) {
	requireSeatbelt(t)
	fresh := func() string {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, root, map[string]string{".env": fakeToken, "cfg/.env": fakeToken, "src/a.go": "package a\n"})
		return root
	}
	attacks := map[string]string{
		"rename the file":                "mv .env y && cat y",
		"rename the file out of its dir": "mv cfg/.env y && cat y",
		"rename the parent directory":    "mv cfg cfg2 && cat cfg2/.env",
		"rename the repository root":     "cd .. && b=$(basename \"$OLDPWD\") && mv \"$b\" \"$b-moved\" && cat \"$b-moved/.env\"",
		"hard link the file":             "ln .env y && cat y",
		"hard link the nested file":      "ln cfg/.env y && cat y",
		"copy the file":                  "cp .env y && cat y",
		"delete the file":                "rm .env && echo deleted",
	}
	for name, command := range attacks {
		root := fresh()
		out, _ := runContained(t, root, command)
		if strings.Contains(out, fakeToken) || strings.Contains(out, "deleted") {
			t.Errorf("%s: the command succeeded under containment: %q", name, out)
		}
		for _, secret := range []string{".env", "cfg/.env"} {
			if body, err := os.ReadFile(filepath.Join(root, secret)); err != nil || string(body) != fakeToken {
				t.Errorf("%s: %s was moved or changed by a contained command: %q, %v", name, secret, body, err)
			}
		}
	}
	// Ordinary work is untouched: write, create, rename and remove elsewhere.
	root := fresh()
	out, code := runContained(t, root, "echo edit >> src/a.go && touch src/new.go && mkdir src/d && mv src/new.go src/d/n.go && rm src/d/n.go && cat src/a.go")
	if code != 0 || !strings.Contains(out, "edit") {
		t.Fatalf("an ordinary write under containment failed (exit %d): %s", code, out)
	}
}
