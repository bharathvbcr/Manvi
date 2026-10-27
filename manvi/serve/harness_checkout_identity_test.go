package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one-live-harness-per-checkout guard keys a checkout by the directory
// itself, not by how its path is spelled: two spellings of one directory are one
// checkout, two directories are two.

// harnessAttempt is an attempt that asks for a harness, registered with the
// runner as Prepare registers one.
func harnessAttempt(r *ManagedRunner, id string) *managedJob {
	job := &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "x"}}
	r.mu.Lock()
	r.jobs[id] = job
	r.mu.Unlock()
	return job
}

// caseAlias returns another spelling of dir that differs in case only, or skips
// the test on a filesystem where that is a different directory.
func caseAlias(t *testing.T, dir string) string {
	t.Helper()
	alias := filepath.Join(filepath.Dir(dir), strings.ToUpper(filepath.Base(dir)))
	if alias == dir {
		alias = filepath.Join(filepath.Dir(dir), strings.ToLower(filepath.Base(dir)))
	}
	if _, err := os.Stat(alias); err != nil {
		t.Skip("this filesystem is case-sensitive, so a differently cased spelling is another directory")
	}
	return alias
}

func TestACaseAliasOfALiveCheckoutIsTheSameCheckout(t *testing.T) {
	base := resolvedDir(t)
	dir := filepath.Join(base, "MyRepo")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	alias := caseAlias(t, dir)
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	first := harnessAttempt(r, "a")
	if err := r.enterCheckout(first, managedRun{Cwd: dir}); err != nil {
		t.Fatal(err)
	}
	first.materialised = &harnessMaterialisation{}
	second := harnessAttempt(r, "b")
	if err := r.enterCheckout(second, managedRun{Cwd: alias}); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("a second run on %s was admitted beside the live one on %s: %v", alias, dir, err)
	}
	if e := r.HarnessBusy(alias); e == nil || e.Code != ErrHarnessBusy {
		t.Fatalf("HarnessBusy(%s) = %v, want the live block on %s reported", alias, e, dir)
	}
}

func TestALinkToALiveCheckoutIsTheSameCheckout(t *testing.T) {
	base := resolvedDir(t)
	dir := filepath.Join(base, "repo")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	first := harnessAttempt(r, "a")
	if err := r.enterCheckout(first, managedRun{Cwd: dir}); err != nil {
		t.Fatal(err)
	}
	second := harnessAttempt(r, "b")
	if err := r.enterCheckout(second, managedRun{Cwd: link}); busyCode(err) != ErrHarnessBusy {
		t.Fatalf("a run through a link to the live checkout was admitted: %v", err)
	}
}

func TestTwoDifferentDirectoriesAreTwoCheckouts(t *testing.T) {
	base := resolvedDir(t)
	one, other := filepath.Join(base, "repo"), filepath.Join(base, "repo2")
	for _, d := range []string{one, other} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	first := harnessAttempt(r, "a")
	if err := r.enterCheckout(first, managedRun{Cwd: one}); err != nil {
		t.Fatal(err)
	}
	first.materialised = &harnessMaterialisation{}
	second := harnessAttempt(r, "b")
	if err := r.enterCheckout(second, managedRun{Cwd: other}); err != nil {
		t.Fatalf("a run on another directory was refused: %v", err)
	}
	if e := r.HarnessBusy(other); e != nil {
		t.Fatalf("a block on %s made %s busy: %v", one, other, e)
	}
}
