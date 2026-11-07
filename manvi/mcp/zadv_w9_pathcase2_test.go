package mcp

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Second adversarial pass over the component-by-component resolveExisting. Every
// expectation about the kernel is taken by opening or creating a file and
// comparing identities, never by reasoning about what the kernel would do.

const sep = string(filepath.Separator)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// sameAsInRepo asserts the precondition: the kernel opens candidate and reaches
// the very file marker, which lives in the repository.
func sameAsInRepo(t *testing.T, candidate, marker string) {
	t.Helper()
	a, err := os.Stat(candidate)
	if err != nil {
		t.Fatalf("precondition: kernel cannot stat %q: %v", candidate, err)
	}
	b, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatalf("precondition: %q is not the repository file %q", candidate, marker)
	}
}

// Attack 1: every spelling that reaches the repository file, checked against the
// kernel.
func TestZAdvW9SecondKernelSpellingsAreInside(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, filepath.Join(repo, "sub", "deep"), filepath.Join(out, "a", "b"), filepath.Join(out, "c"))
	marker := filepath.Join(repo, "sub", "m.yaml")
	if err := os.WriteFile(marker, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	// relative targets
	link(t, "../../../repo/sub", filepath.Join(out, "a", "b", "rel-up"))
	link(t, "./../../../repo/sub/", filepath.Join(out, "a", "b", "rel-dot-slash"))
	link(t, "x/../../../repo/sub", filepath.Join(out, "a", "b", "rel-x-dotdot")) // x absent: kernel ENOENT, kept apart below
	link(t, filepath.Join(repo, "sub")+sep, filepath.Join(out, "abs-trail"))
	link(t, filepath.Join(repo, "sub"), filepath.Join(out, "abs"))
	link(t, repo+"/./sub/../sub", filepath.Join(out, "abs-weird"))
	// target that itself contains .. after a symlink: out/c/hop -> ../a/b/rel-up/.. = repo
	link(t, "../a/b/rel-up/..", filepath.Join(out, "c", "hop"))
	// chain across directories: l1 -> ../c/l2, l2 -> ../a/b/l3, l3 -> ../../../repo/sub
	link(t, "../../../repo/sub", filepath.Join(out, "a", "b", "l3"))
	link(t, "../a/b/l3", filepath.Join(out, "c", "l2"))
	link(t, "c/l2", filepath.Join(out, "l1"))
	// link to link to dangling path that becomes the repo file's directory
	link(t, "../repo/sub/new", filepath.Join(out, "dang-target"))
	link(t, "dang-target", filepath.Join(out, "dang2"))
	// root reached through a symlink
	link(t, repo, filepath.Join(base, "repo-alias"))

	m := "m.yaml"
	ok := map[string]string{
		"rel-up":         filepath.Join(out, "a", "b", "rel-up", m),
		"rel-dot-slash":  filepath.Join(out, "a", "b", "rel-dot-slash", m),
		"abs-trail":      filepath.Join(out, "abs-trail", m),
		"abs":            filepath.Join(out, "abs", m),
		"abs-weird":      filepath.Join(out, "abs-weird", m),
		"hop-dotdot":     filepath.Join(out, "c", "hop", "sub", m),
		"chain":          filepath.Join(out, "l1", m),
		"dots":           out + "/./a/./b/./rel-up/./" + m,
		"repeated-slash": out + "///a//b///rel-up//" + m,
		"dotdot-after":   filepath.Join(out, "a", "b", "rel-up") + "/../sub/" + m,
		"dotdot-twice":   filepath.Join(out, "a", "b", "rel-up") + "/../../repo/sub/" + m,
		"above-root":     "/../../../.." + filepath.Join(repo, "sub", m),
		"alias":          filepath.Join(base, "repo-alias", "sub", m),
		"trailing-slash": filepath.Join(out, "abs") + "/" + m + "/",
	}
	for name, cand := range ok {
		if name == "trailing-slash" {
			// a file with a trailing slash is ENOTDIR for the kernel; only inside-ness of the spelling matters
			if inside, err := PathIsInside(repo, cand); err == nil && !inside {
				t.Errorf("%s: (false, nil) for %q", name, cand)
			}
			continue
		}
		sameAsInRepo(t, cand, marker)
		mustInside(t, repo, cand, true)
		mustInside(t, filepath.Join(base, "repo-alias"), cand, true)
	}
	// root spelled through an alias, candidate by plain path
	mustInside(t, filepath.Join(base, "repo-alias"), marker, true)
	// candidate is the root itself, in several spellings
	for _, c := range []string{repo, repo + sep, repo + "/.", repo + "/sub/..", filepath.Join(base, "repo-alias"), out + "/abs/.."} {
		if c == out+"/abs/.." {
			// abs -> repo/sub, so the kernel's answer for abs/.. is repo
			a, _ := os.Stat(c)
			r, _ := os.Stat(repo)
			if !os.SameFile(a, r) {
				t.Fatalf("precondition: %q is not repo", c)
			}
		}
		mustInside(t, repo, c, true)
	}
	// Creating the file through the dangling chain lands inside the repo.
	nf := filepath.Join(out, "dang2", "f.yaml")
	if _, err := PathIsInside(repo, nf); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(repo, "sub", "new"))
	if err := os.WriteFile(nf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "sub", "new", "f.yaml")); err != nil {
		t.Fatalf("precondition: creating via dangling chain did not land in repo: %v", err)
	}
	mustInside(t, repo, nf, true)
}

// The dangling chain checked BEFORE the target exists, then the kernel's create.
func TestZAdvW9SecondDanglingChainBeforeCreate(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, filepath.Join(repo, "sub"), out)
	link(t, "../repo/sub/new", filepath.Join(out, "t1"))
	link(t, "t1", filepath.Join(out, "t2"))
	link(t, "t2/", filepath.Join(out, "t3"))
	mustInside(t, repo, filepath.Join(out, "t3"), true)
	mustInside(t, repo, filepath.Join(out, "t3", "f.yaml"), true)
	// target spelled with .. after a dangling link: t4 -> t1/../x ; the kernel fails now, so
	// only the "once t1 exists as a dir" state is checked.
	link(t, "t1/../zz", filepath.Join(out, "t4"))
	mkdirs(t, filepath.Join(repo, "sub", "new"))
	if err := os.WriteFile(filepath.Join(out, "t4"), []byte("x"), 0o600); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "sub", "zz")); err != nil {
		t.Fatalf("precondition: expected repo/sub/zz: %v", err)
	}
	mustInside(t, repo, filepath.Join(out, "t4"), true)
}

// FINDING candidate: past the first absent component the resolver stops following
// links, but a ".." in the absent tail can climb back out of the absent part onto
// the existing tree, where the next component may be a symlink into the repo.
// Once the absent directory exists the kernel opens inside the repo.
func TestZAdvW9SecondDotDotOutOfAbsentTailThenSymlink(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, filepath.Join(repo, "sub"), out)
	link(t, filepath.Join(repo, "sub"), filepath.Join(out, "into"))
	cand := out + "/absent/../into/f.yaml"
	got, err := PathIsInside(repo, cand)
	// Prove what the kernel does once the absent directory exists.
	mkdirs(t, filepath.Join(out, "absent"))
	if err := os.WriteFile(cand, []byte("x"), 0o600); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(repo, "sub", "f.yaml")); serr != nil {
		t.Fatalf("precondition: kernel did not create the file in the repo: %v", serr)
	}
	if err == nil && !got {
		t.Errorf("PathIsInside(%q) = (false, nil) before the absent dir exists; the kernel creates the file inside the repo once it does", cand)
	}
}

// Same shape with the ".." arriving through a dangling symlink target.
func TestZAdvW9SecondDotDotInDanglingTargetThenSymlink(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, filepath.Join(repo, "sub"), out)
	link(t, filepath.Join(repo, "sub"), filepath.Join(out, "into"))
	link(t, "gone/../into", filepath.Join(out, "d"))
	cand := filepath.Join(out, "d", "f.yaml")
	got, err := PathIsInside(repo, cand)
	mkdirs(t, filepath.Join(out, "gone"))
	if werr := os.WriteFile(cand, []byte("x"), 0o600); werr != nil {
		t.Fatalf("precondition: %v", werr)
	}
	if _, serr := os.Stat(filepath.Join(repo, "sub", "f.yaml")); serr != nil {
		t.Fatalf("precondition: %v", serr)
	}
	if err == nil && !got {
		t.Errorf("PathIsInside(%q) = (false, nil); once out/gone exists the kernel creates the file in the repo", cand)
	}
}

// Attack 2: ENOTDIR.
func TestZAdvW9SecondThroughRegularFile(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, repo, out)
	if err := os.WriteFile(filepath.Join(out, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "rfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Outside regular file: outside (the kernel cannot open it; it is not a repo path).
	mustInside(t, repo, filepath.Join(out, "file", "child", "f.yaml"), false)
	// Inside regular file: inside.
	mustInside(t, repo, filepath.Join(repo, "rfile", "child"), true)
	// Lexically outside, resolved prefix inside: out/lnk -> repo/rfile (a file), then child.
	link(t, filepath.Join(repo, "rfile"), filepath.Join(out, "lnk"))
	mustInside(t, repo, filepath.Join(out, "lnk", "child"), true)
	// A ".." taken above a regular file: kernel ENOTDIR; must not become a false outside of
	// something inside. out/file/../../repo/x names repo/x lexically.
	if inside, err := PathIsInside(repo, out+"/file/../../repo/x"); err == nil && !inside {
		t.Errorf("(false, nil) for a spelling that lexically names repo/x")
	}
	// Through a file inside the repo with a ".." that exits the repo.
	if inside, err := PathIsInside(repo, repo+"/rfile/../../out/x"); err == nil && inside {
		t.Logf("note: (true) for repo/rfile/../../out/x, a false inside (fail-safe)")
	}
	// Previously an error (fail-closed for checkoutsOverlap); record the new answers.
	t.Logf("outside file child -> inside=%v", func() bool { b, _ := PathIsInside(repo, filepath.Join(out, "file", "child")); return b }())
}

// Attack 3: loops and the cap, each time-bounded.
func TestZAdvW9SecondLoopsAndCap(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	mkdirs(t, repo, out)
	link(t, "b", filepath.Join(out, "a"))
	link(t, "a", filepath.Join(out, "b"))
	link(t, "self", filepath.Join(out, "self"))
	link(t, "self2/..", filepath.Join(out, "self2"))
	mkdirs(t, filepath.Join(out, "dir"))
	run := func(name string, f func() (bool, error)) (bool, error) {
		type res struct {
			b bool
			e error
		}
		ch := make(chan res, 1)
		go func() { b, e := f(); ch <- res{b, e} }()
		select {
		case r := <-ch:
			return r.b, r.e
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: did not return in 5s", name)
			return false, nil
		}
	}
	for _, n := range []string{"a", "b", "self", "self2"} {
		cand := filepath.Join(out, n, "f")
		in, err := run(n, func() (bool, error) { return PathIsInside(repo, cand) })
		if err == nil && !in {
			t.Errorf("loop %s: (false, nil): a loop is unresolved and must not read as clean outside", n)
		}
	}
	// chain of N links: l1 -> l2 -> ... -> lN -> repo (absolute target).
	chain := func(n int, dir string) string {
		mkdirs(t, dir)
		for i := 1; i <= n; i++ {
			tgt := fmt.Sprintf("l%d", i+1)
			if i == n {
				tgt = repo
			}
			link(t, tgt, filepath.Join(dir, fmt.Sprintf("l%d", i)))
		}
		return filepath.Join(dir, "l1")
	}
	for _, n := range []int{39, 40, 41, 42} {
		head := chain(n, filepath.Join(out, fmt.Sprintf("chain%d", n)))
		_, kerr := os.Stat(head)
		in, err := run("chain", func() (bool, error) { return PathIsInside(repo, head) })
		t.Logf("chain of %d links: kernel stat err=%v ; PathIsInside=(%v, %v)", n, kerr, in, err)
		if kerr == nil && (err != nil || !in) {
			t.Errorf("chain of %d: the kernel opens the repo but PathIsInside=(%v, %v)", n, in, err)
		}
		if kerr != nil && err == nil && !in {
			t.Errorf("chain of %d: kernel refuses (%v) and PathIsInside says clean outside", n, kerr)
		}
	}
	// The count is of links followed along the whole walk; a path crossing 2 chains of 21 trips it.
	d1 := chain(21, filepath.Join(out, "c21a"))
	d2 := chain(21, filepath.Join(out, "c21b"))
	// d1 -> repo ; place a link in repo that goes to c21b's head, so walking crosses 21 + 1 + 21.
	link(t, d2, filepath.Join(repo, "x"))
	cand := filepath.Join(d1, "x", "f")
	_, kerr := os.Stat(cand)
	in, err := PathIsInside(repo, cand)
	t.Logf("two chains of 21 (43 links total): kernel err=%v ; PathIsInside=(%v, %v)", kerr, in, err)
	if err == nil && !in {
		t.Errorf("(false, nil) for a path that goes through repo links")
	}
}

// Attack 4: swap a component between a symlink and a directory during the walk.
func TestZAdvW9SecondRaceSwapLinkAndDir(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	mkdirs(t, filepath.Join(repo, "real", "sub"), filepath.Join(repo, "other", "sub"))
	comp := filepath.Join(repo, "swap")
	link(t, "real", comp)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := filepath.Join(repo, "swap.tmp")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 3 {
			case 0: // symlink to dir, atomically
				_ = os.Remove(tmp)
				if os.Symlink("other", tmp) == nil {
					_ = os.Rename(tmp, comp)
				}
			case 1: // symlink with an absolute target
				_ = os.Remove(tmp)
				if os.Symlink(filepath.Join(repo, "real"), tmp) == nil {
					_ = os.Rename(tmp, comp)
				}
			case 2: // real directory (remove link then mkdir: a gap where it is absent)
				if fi, err := os.Lstat(comp); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					_ = os.Remove(comp)
					_ = os.MkdirAll(filepath.Join(comp, "sub"), 0o750)
				}
			}
			if i%3 == 2 {
				_ = os.RemoveAll(comp) // then recreate as link on next round
				_ = os.Symlink("real", comp)
			}
		}
	}()
	cands := []string{
		filepath.Join(comp, "sub", "f.yaml"),
		filepath.Join(comp, "..", "real", "sub", "f"),
		filepath.Join(comp, "sub", "..", "sub", "new", "f"),
	}
	deadline := time.Now().Add(3 * time.Second)
	outside, errs, n := 0, map[string]int{}, 0
	var first string
	for time.Now().Before(deadline) {
		for _, c := range cands {
			n++
			in, err := PathIsInside(repo, c)
			if err != nil {
				errs[err.Error()]++
				continue
			}
			if !in {
				outside++
				if first == "" {
					first = c
				}
			}
		}
	}
	close(stop)
	wg.Wait()
	for e, c := range errs {
		t.Logf("race error (fail-closed for every caller) x%d: %s", c, e)
	}
	if outside > 0 {
		t.Errorf("%d of %d answers were (false, nil) for paths inside in every state; first %q", outside, n, first)
	}
}

// Attack 5: property test against the kernel.
const propSeed = 20261107

func TestZAdvW9SecondPropertyAgainstKernel(t *testing.T) {
	cases := 4000
	if testing.Short() {
		cases = 500
	}
	type mismatch struct {
		seed         int64
		cand         string
		want, got    bool
		err          error
		tree         string
		kernelDetail string
	}
	var mism []mismatch
	checked, kernelOutside, kernelInside, errCount := 0, 0, 0, 0
	for i := 0; i < cases; i++ {
		seed := int64(propSeed) + int64(i)
		rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- a fixed-seed sweep: reproducing a failing draw is the point, secrecy is not.
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(base, "repo")
		tree := buildRandomTree(t, rng, base, repo)
		inRepo := repoIdentities(t, repo)
		for k := 0; k < 12; k++ {
			cand := randomCandidate(rng, base, repo)
			info, serr := os.Stat(cand)
			if serr != nil {
				continue // only candidates the kernel resolves are checkable against it
			}
			want := false
			for _, ri := range inRepo {
				if os.SameFile(ri, info) {
					want = true
					break
				}
			}
			if info.IsDir() {
				// absent tails: a name that does not exist, with and without a ".." that returns to cand.
				for _, tail := range []string{"/zz-absent/new", "/zz-absent/../new", "/zz-absent/../../" + filepath.Base(base) + "/zz2"} {
					if tail != "/zz-absent/new" && tail != "/zz-absent/../new" {
						continue
					}
					tg, terr := PathIsInside(repo, cand+tail)
					checked++
					if terr != nil || tg != want {
						mism = append(mism, mismatch{seed: seed, cand: cand + tail, want: want, got: tg, err: terr, tree: tree})
					}
				}
			}
			got, perr := PathIsInside(repo, cand)
			checked++
			if want {
				kernelInside++
			} else {
				kernelOutside++
			}
			if perr != nil {
				errCount++
				if want { // an error is fail-closed, but note that it should not occur for a path the kernel opens
					mism = append(mism, mismatch{seed: seed, cand: cand, want: want, got: got, err: perr, tree: tree})
				}
				continue
			}
			if got != want {
				mism = append(mism, mismatch{seed: seed, cand: cand, want: want, got: got, tree: tree})
			}
		}
	}
	t.Logf("property: %d cases (seeds %d..%d), %d checkable candidates (%d kernel-inside, %d kernel-outside), %d errors, %d mismatches",
		cases, propSeed, propSeed+cases-1, checked, kernelInside, kernelOutside, errCount, len(mism))
	for i, m := range mism {
		if i >= 8 {
			break
		}
		t.Errorf("seed %d: PathIsInside(repo, %q) = (%v, %v), kernel says inside=%v\ntree:\n%s", m.seed, m.cand, m.got, m.err, m.want, m.tree)
	}
}

var propNames = []string{"a", "b", "c", "d", "A", "B", "repo", "REPO", "out"}

func buildRandomTree(t *testing.T, rng *rand.Rand, base, repo string) string {
	t.Helper()
	var desc []string
	dirs := []string{base, repo}
	mk := func(parent string) {
		name := propNames[rng.Intn(4)]
		p := filepath.Join(parent, name)
		if _, err := os.Lstat(p); err == nil {
			return
		}
		if os.Mkdir(p, 0o750) == nil {
			dirs = append(dirs, p)
			desc = append(desc, "dir  "+p)
		}
	}
	if err := os.Mkdir(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5+rng.Intn(6); i++ {
		mk(dirs[rng.Intn(len(dirs))])
	}
	for i := 0; i < 3; i++ {
		p := filepath.Join(dirs[rng.Intn(len(dirs))], "f"+fmt.Sprint(i))
		if os.WriteFile(p, []byte("x"), 0o600) == nil {
			desc = append(desc, "file "+p)
		}
	}
	for i := 0; i < 6+rng.Intn(8); i++ {
		parent := dirs[rng.Intn(len(dirs))]
		name := "l" + fmt.Sprint(i)
		var target string
		switch rng.Intn(7) {
		case 4, 5: // relative path to an existing dir, built so it resolves
			rel, err := filepath.Rel(parent, dirs[rng.Intn(len(dirs))])
			if err != nil {
				rel = "."
			}
			target = rel
			if rng.Intn(3) == 0 {
				target += "/"
			}
		case 6: // absolute path to an existing dir
			target = dirs[rng.Intn(len(dirs))]
		case 0: // absolute to a dir (maybe dangling)
			target = filepath.Join(dirs[rng.Intn(len(dirs))], propNames[rng.Intn(4)])
		case 1: // relative, climbing
			target = strings.Repeat("../", rng.Intn(3)) + randomRel(rng)
		case 2: // relative with a link name inside, then ..
			target = randomRel(rng) + "/../" + randomRel(rng)
		default:
			target = randomRel(rng)
			if rng.Intn(2) == 0 {
				target += "/"
			}
		}
		if os.Symlink(target, filepath.Join(parent, name)) == nil {
			desc = append(desc, fmt.Sprintf("link %s -> %s", filepath.Join(parent, name), target))
		}
	}
	return strings.Join(desc, "\n")
}

func randomRel(rng *rand.Rand) string {
	n := 1 + rng.Intn(3)
	parts := make([]string, n)
	for i := range parts {
		switch rng.Intn(8) {
		case 0:
			parts[i] = ".."
		case 1:
			parts[i] = "."
		case 2:
			parts[i] = "l" + fmt.Sprint(rng.Intn(10))
		default:
			parts[i] = propNames[rng.Intn(4)]
		}
	}
	return strings.Join(parts, "/")
}

func randomCandidate(rng *rand.Rand, base, repo string) string {
	start := []string{base, repo, base + "/", repo + "//", "/"}[rng.Intn(5)]
	n := 1 + rng.Intn(8)
	parts := []string{start}
	for i := 0; i < n; i++ {
		switch rng.Intn(10) {
		case 0:
			parts = append(parts, "..")
		case 1:
			parts = append(parts, ".")
		case 2, 3:
			parts = append(parts, "l"+fmt.Sprint(rng.Intn(14)))
		case 4:
			parts = append(parts, propNames[4+rng.Intn(len(propNames)-4)])
		case 5:
			parts = append(parts, "f"+fmt.Sprint(rng.Intn(3)))
		default:
			parts = append(parts, propNames[rng.Intn(4)])
		}
	}
	c := strings.Join(parts, "/")
	if rng.Intn(6) == 0 {
		c += "/"
	}
	return c
}

// repoIdentities lists the file identity of every entry at or under repo, without
// following symlinks.
func repoIdentities(t *testing.T, repo string) []fs.FileInfo {
	t.Helper()
	var out []fs.FileInfo
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk: an entry that cannot be read is skipped, not a failure
		}
		if fi, e := os.Lstat(p); e == nil {
			out = append(out, fi)
		}
		return nil
	})
	return out
}
