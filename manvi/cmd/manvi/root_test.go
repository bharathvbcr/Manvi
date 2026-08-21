package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"manvi/flags"
)

func TestFindProjectRoot_GitRepo(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	gitDir := filepath.Join(tmp, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatalf("failed to create .git: %v", err)
	}

	deepSub := filepath.Join(tmp, "a", "b", "c", "d", "e")
	if err := os.MkdirAll(deepSub, 0o755); err != nil {
		t.Fatalf("failed to create deepSub: %v", err)
	}

	for _, sub := range []string{
		tmp,
		filepath.Join(tmp, "a"),
		filepath.Join(tmp, "a", "b"),
		filepath.Join(tmp, "a", "b", "c"),
		deepSub,
	} {
		root := findProjectRoot(sub)
		if root != tmp {
			t.Errorf("findProjectRoot(%q) = %q; want %q", sub, root, tmp)
		}
	}
}

func TestFindProjectRoot_GitWorktree(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	gitFile := filepath.Join(tmp, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /path/to/main/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("failed to create .git file: %v", err)
	}

	sub := filepath.Join(tmp, "pkg", "core")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub: %v", err)
	}

	root := findProjectRoot(sub)
	if root != tmp {
		t.Errorf("findProjectRoot in worktree = %q; want %q", root, tmp)
	}
}

func TestFindProjectRoot_DevCouncilDir(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	dcDir := filepath.Join(tmp, ".devcouncil")
	if err := os.Mkdir(dcDir, 0o755); err != nil {
		t.Fatalf("failed to create .devcouncil: %v", err)
	}

	sub := filepath.Join(tmp, "cmd", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub: %v", err)
	}

	root := findProjectRoot(sub)
	if root != tmp {
		t.Errorf("findProjectRoot with .devcouncil = %q; want %q", root, tmp)
	}
}

func TestFindProjectRoot_ManifestMarkers(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	goMod := filepath.Join(tmp, "go.mod")
	if err := os.WriteFile(goMod, []byte("module example.com/test\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatalf("failed to create go.mod: %v", err)
	}

	sub := filepath.Join(tmp, "internal", "service")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub: %v", err)
	}

	root := findProjectRoot(sub)
	if root != tmp {
		t.Errorf("findProjectRoot with go.mod = %q; want %q", root, tmp)
	}
}

func TestFindProjectRoot_NoMarkersFallback(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	sub := filepath.Join(tmp, "isolated", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub: %v", err)
	}

	root := findProjectRoot(sub)
	if root != sub {
		t.Errorf("findProjectRoot without markers = %q; want %q", root, sub)
	}
}

func TestFindProjectRoot_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	custom := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}

	t.Setenv("MANVI_PROJECT_ROOT", custom)
	root := findProjectRoot(tmp)
	if root != custom {
		t.Errorf("findProjectRoot with MANVI_PROJECT_ROOT = %q; want %q", root, custom)
	}
}

func TestResolveCLIPath(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	pkgDir := filepath.Join(root, "pkg", "calc")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("failed to create pkgDir: %v", err)
	}

	calcFile := filepath.Join(pkgDir, "calc.go")
	if err := os.WriteFile(calcFile, []byte("package calc\n"), 0o644); err != nil {
		t.Fatalf("failed to create calc.go: %v", err)
	}

	rootFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(rootFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to create main.go: %v", err)
	}

	cwd := pkgDir

	// 1. File exists in cwd: "calc.go"
	res := resolveCLIPath(root, cwd, "calc.go")
	if res != calcFile {
		t.Errorf("resolveCLIPath(calc.go) = %q; want %q", res, calcFile)
	}

	// 2. Repo-relative existing file: "main.go" while standing in cwd
	res = resolveCLIPath(root, cwd, "main.go")
	if res != rootFile {
		t.Errorf("resolveCLIPath(main.go) = %q; want %q", res, rootFile)
	}

	// 3. Repo-relative existing path: "pkg/calc/calc.go" while standing in cwd
	res = resolveCLIPath(root, cwd, "pkg/calc/calc.go")
	if res != calcFile {
		t.Errorf("resolveCLIPath(pkg/calc/calc.go) = %q; want %q", res, calcFile)
	}

	// 4. Absolute path: calcFile
	res = resolveCLIPath(root, cwd, calcFile)
	if res != calcFile {
		t.Errorf("resolveCLIPath(abs) = %q; want %q", res, calcFile)
	}

	// 5. New file in cwd: "new_helper.go"
	res = resolveCLIPath(root, cwd, "new_helper.go")
	expectedNewCwd := filepath.Join(cwd, "new_helper.go")
	if res != expectedNewCwd {
		t.Errorf("resolveCLIPath(new_helper.go) = %q; want %q", res, expectedNewCwd)
	}

	// 6. New file with repo-relative path: "pkg/calc/new_helper.go" (where pkg exists in root)
	res = resolveCLIPath(root, cwd, "pkg/calc/new_helper.go")
	expectedNewRoot := filepath.Join(root, "pkg", "calc", "new_helper.go")
	if res != expectedNewRoot {
		t.Errorf("resolveCLIPath(pkg/calc/new_helper.go) = %q; want %q", res, expectedNewRoot)
	}
}

func TestStateDirAnchoredToProjectRoot(t *testing.T) {
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	cleanup := setProjectRootForTest(tmp)
	defer cleanup()

	expectedDC := filepath.Join(tmp, ".devcouncil")
	if sd := stateDir(); sd != expectedDC {
		t.Errorf("stateDir() = %q; want %q", sd, expectedDC)
	}

	expectedCfg := filepath.Join(expectedDC, "config.yaml")
	if cp := configPath(); cp != expectedCfg {
		t.Errorf("configPath() = %q; want %q", cp, expectedCfg)
	}

	expectedDB := filepath.Join(expectedDC, "state.sqlite")
	if db := storeDBPath(); db != expectedDB {
		t.Errorf("storeDBPath() = %q; want %q", db, expectedDB)
	}

	expectedGraph := filepath.Join(expectedDC, "code_graph.json")
	if gp := graphArtifactPath(); gp != expectedGraph {
		t.Errorf("graphArtifactPath() = %q; want %q", gp, expectedGraph)
	}

	expectedSessions := filepath.Join(expectedDC, "sessions")
	if sp := sessionsDir(); sp != expectedSessions {
		t.Errorf("sessionsDir() = %q; want %q", sp, expectedSessions)
	}

	expectedArtifacts := filepath.Join(expectedDC, "artifacts")
	if ap := artifactsDir(); ap != expectedArtifacts {
		t.Errorf("artifactsDir() = %q; want %q", ap, expectedArtifacts)
	}
}

func TestScaffoldFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatalf("failed to create .git: %v", err)
	}

	sub := filepath.Join(root, "pkg", "deep", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub: %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cleanup := setProjectRootForTest(root)
	defer cleanup()

	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatalf("flags registry: %v", err)
	}

	report, err := scaffold(reg)
	if err != nil {
		t.Fatalf("scaffold failed: %v", err)
	}

	if report.Root != root {
		t.Errorf("scaffold report.Root = %q; want %q", report.Root, root)
	}

	// Verify .devcouncil exists at root
	if _, err := os.Stat(filepath.Join(root, ".devcouncil")); os.IsNotExist(err) {
		t.Errorf(".devcouncil was not created at root %q", root)
	}

	// Verify .devcouncil does NOT exist in sub
	if _, err := os.Stat(filepath.Join(sub, ".devcouncil")); !os.IsNotExist(err) {
		t.Errorf(".devcouncil was erroneously created in subdirectory %q", sub)
	}

	// Verify .gitignore does NOT exist in sub
	if _, err := os.Stat(filepath.Join(sub, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf(".gitignore was erroneously created in subdirectory %q", sub)
	}
}

func TestCheckAndAllow_FromSubdirectory(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}

	sub := filepath.Join(root, "src", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("create sub: %v", err)
	}

	mainFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("create main.go: %v", err)
	}

	subFile := filepath.Join(sub, "helper.go")
	if err := os.WriteFile(subFile, []byte("package sub\n"), 0o644); err != nil {
		t.Fatalf("create helper.go: %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cleanup := setProjectRootForTest(root)
	defer cleanup()

	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	gate, _, err := buildGate(reg)
	if err != nil {
		t.Fatalf("buildGate: %v", err)
	}

	// Check helper.go from sub directory
	var buf bytes.Buffer
	if err := check(&buf, gate, []string{"helper.go"}); err != nil {
		t.Fatalf("check helper.go: %v", err)
	}
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("ALLOW")) && !bytes.Contains(buf.Bytes(), []byte("DENY")) {
		t.Errorf("unexpected check output: %s", out)
	}
	if bytes.Contains(buf.Bytes(), []byte("path.outside_root")) {
		t.Errorf("helper.go was erroneously flagged as path.outside_root: %s", out)
	}

	// Check main.go from sub directory (relative to root)
	buf.Reset()
	if err := check(&buf, gate, []string{"main.go"}); err != nil {
		t.Fatalf("check main.go: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("path.outside_root")) {
		t.Errorf("main.go was erroneously flagged as path.outside_root: %s", buf.String())
	}
}

// The project-root walk in pass 1 is unbounded: it climbs to the filesystem
// root looking for .devcouncil/.git before pass 2 ever gets to consider a
// build manifest. That ordering is right inside a repository — a repo's .git
// sits above a nested package's go.mod, and the repo is the root we want — but
// it also means a marker in a directory shared by unrelated work outranks the
// project you are actually standing in.
//
// Observed: a stray /private/tmp/.devcouncil left by an earlier session made a
// module under /private/tmp/.../sandbox resolve its root to /private/tmp. The
// root is what bounds the outside-root hard rule, so under --yolo, where that
// rule is off and nothing else contains the agent, the blast radius became
// every sibling directory in /tmp. The agent duly listed 99 of them and read
// other sessions' scratch directories.
//
// A directory that is shared by construction — the filesystem root, the home
// directory itself, the system temp directories — is never the root of the
// project you are editing. A marker there is debris, and adopting it widens
// containment rather than establishing it.

func TestFindProjectRoot_StrayMarkerInHomeDoesNotSwallowProject(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)

	// A dotfiles repository in the home directory: extremely common, and
	// nothing to do with the project below it.
	if err := os.Mkdir(filepath.Join(home, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}

	project := filepath.Join(home, "Code", "thing")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if root := findProjectRoot(project); root != project {
		t.Errorf("findProjectRoot = %q; want %q — a stray marker in $HOME outranked the module's own go.mod, "+
			"which widens the containment boundary to the whole home directory", root, project)
	}
}

func TestFindProjectRoot_StrayGitInHomeDoesNotSwallowProject(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)

	if err := os.Mkdir(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	project := filepath.Join(home, "scratch", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "Cargo.toml"), []byte("[package]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if root := findProjectRoot(project); root != project {
		t.Errorf("findProjectRoot = %q; want %q — a dotfiles .git in $HOME became the project root", root, project)
	}
}

// The home directory is refused as a root, not as a prefix. A real project
// that happens to live under $HOME keeps its own marker.
func TestFindProjectRoot_MarkerBelowHomeStillWins(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)

	if err := os.Mkdir(filepath.Join(home, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(home, "Code", "repo")
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// From deep inside the repo, the repo's own .git is the answer — and it
	// still outranks a go.mod nested below it, which is the monorepo case the
	// two-pass order exists to serve.
	if err := os.WriteFile(filepath.Join(repo, "pkg", "deep", "go.mod"), []byte("module deep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if root := findProjectRoot(filepath.Join(repo, "pkg", "deep")); root != repo {
		t.Errorf("findProjectRoot = %q; want %q — the repository's own .git must still win over a nested go.mod", root, repo)
	}
}

// Nothing at all below a shared directory: the walk must not fall back on the
// shared directory either. startDir is the honest answer.
func TestFindProjectRoot_NoMarkerBelowHomeFallsBackToStartDir(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)

	if err := os.Mkdir(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	bare := filepath.Join(home, "notes", "scratch")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	if root := findProjectRoot(bare); root != bare {
		t.Errorf("findProjectRoot = %q; want %q — with no marker of its own, a directory under a shared "+
			"root must contain itself rather than inherit the shared root", root, bare)
	}
}

// --- adversarial cases against the shared-root bound ---
//
// isSharedRoot decides where containment starts, so it is worth attacking
// directly. Each case below is a way the environment it reads can be strange,
// absent, or hostile; none of them may panic, and none may quietly hand back a
// root wider than the caller's own directory.

func TestFindProjectRoot_SharedRootBoundIsRobust(t *testing.T) {
	for _, tc := range []struct {
		name  string
		home  string // "" means unset the variable entirely
		tmp   string
		unset bool
	}{
		{name: "home unset", unset: true},
		{name: "home is the filesystem root", home: "/", tmp: "/"},
		{name: "home is relative", home: "relative/path", tmp: "also/relative"},
		{name: "home does not exist", home: "/nonexistent/ghost/home", tmp: "/nonexistent/ghost/tmp"},
		{name: "home is a trailing-slash spelling", home: "/tmp/", tmp: "/tmp/"},
		{name: "home contains dot segments", home: "/tmp/../tmp", tmp: "/var/./tmp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Build the tree first: t.TempDir() reads TMPDIR, so overriding the
			// environment before this point breaks the fixture rather than the
			// code under test.
			project := t.TempDir()
			if resolved, err := filepath.EvalSymlinks(project); err == nil {
				project = resolved
			}
			if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			sub := filepath.Join(project, "a", "b")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}

			if tc.unset {
				t.Setenv("HOME", "")
				os.Unsetenv("HOME")
				t.Setenv("TMPDIR", "")
				os.Unsetenv("TMPDIR")
			} else {
				t.Setenv("HOME", tc.home)
				t.Setenv("TMPDIR", tc.tmp)
			}

			root := findProjectRoot(sub) // must not panic
			if root != project {
				t.Errorf("findProjectRoot = %q; want %q", root, project)
			}
		})
	}
}

// A symlinked project directory must resolve to the same root as the real one,
// or the boundary depends on which spelling the operator typed.
func TestFindProjectRoot_SymlinkedProjectResolvesToTheSameRoot(t *testing.T) {
	real := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(real); err == nil {
		real = resolved
	}
	if err := os.Mkdir(filepath.Join(real, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got, want := findProjectRoot(link), findProjectRoot(sub); got != want {
		t.Errorf("findProjectRoot via symlink = %q; via real path = %q — the containment boundary must not "+
			"depend on which spelling of the path the operator used", got, want)
	}
}

// Running the harness deliberately inside a shared directory must still work.
// The bound stops a deep directory *inheriting* a shared ancestor; it does not
// forbid standing in one.
func TestFindProjectRoot_StandingInTheSharedRootStillWorks(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}

	if root := findProjectRoot(home); root != home {
		t.Errorf("findProjectRoot(home) = %q; want %q — the harness must still run in a directory the "+
			"bound refuses to let others inherit", root, home)
	}
}

// The environment override outranks the bound: an operator naming a root means
// it, even if it is one of the refused directories.
func TestFindProjectRoot_EnvOverrideOutranksTheSharedRootBound(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("MANVI_PROJECT_ROOT", home)

	project := filepath.Join(home, "Code", "thing")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if root := findProjectRoot(project); root != home {
		t.Errorf("findProjectRoot = %q; want %q — an explicit MANVI_PROJECT_ROOT is a deliberate choice "+
			"and the bound must not silently override it", root, home)
	}
}

// An override made of whitespace is not a path anyone meant.
//
// It anchors the state directory, the git boundary and the policy gates, and
// "   " resolved to "<cwd>/   " — a directory the harness would then create and
// treat as the repository. Found by boundary-probing the override, which
// accepted anything.
func TestAWhitespaceProjectRootOverrideIsIgnored(t *testing.T) {
	real := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(real); err == nil {
		real = resolved
	}
	if err := os.Mkdir(filepath.Join(real, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, blank := range []string{"   ", "\t", "\n", " \t\n "} {
		t.Setenv("MANVI_PROJECT_ROOT", blank)
		if got := findProjectRoot(sub); got != real {
			t.Errorf("MANVI_PROJECT_ROOT=%q gave root %q; a whitespace override must fall through "+
				"to discovery, not anchor the harness at a directory made of spaces", blank, got)
		}
	}
}
