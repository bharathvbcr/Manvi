//go:build unix

package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial tests for devcouncil_load_skill (dh-2). Each one states the
// property a hostile repository must not be able to violate.

const advSecret = "SECRET-MARKER-9f3a"

func advLoad(f *skillFixture, name string) tools.Result {
	return f.call("devcouncil_load_skill", map[string]string{"name": name})
}

func advMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestAdvContainmentNeverLeaksSecret drives every symlink / hard link shape and
// requires that the secret never reaches the tool result.
func TestAdvContainmentNeverLeaksSecret(t *testing.T) {
	outside := t.TempDir()
	advMust(t, os.WriteFile(filepath.Join(outside, "SKILL.md"),
		[]byte("---\nname: foo\ndescription: d\n---\n"+advSecret+"\n"), 0o600))
	advMust(t, os.MkdirAll(filepath.Join(outside, "skills", "foo"), 0o750))
	advMust(t, os.WriteFile(filepath.Join(outside, "skills", "foo", "SKILL.md"),
		[]byte("---\nname: foo\ndescription: d\n---\n"+advSecret+"\n"), 0o600))

	secretSkill := "---\nname: foo\ndescription: d\n---\n" + advSecret + "\n"

	cases := map[string]func(t *testing.T, root string){
		"dot-agents is symlink outside": func(t *testing.T, root string) {
			advMust(t, os.Symlink(outside, filepath.Join(root, ".agents")))
		},
		"skills dir is symlink outside": func(t *testing.T, root string) {
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents"), 0o750))
			advMust(t, os.Symlink(filepath.Join(outside, "skills"), filepath.Join(root, ".agents", "skills")))
		},
		"skill dir is symlink outside": func(t *testing.T, root string) {
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o750))
			advMust(t, os.Symlink(filepath.Join(outside, "skills", "foo"), filepath.Join(root, ".agents", "skills", "foo")))
		},
		"SKILL.md is symlink outside": func(t *testing.T, root string) {
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills", "foo"), 0o750))
			advMust(t, os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(root, ".agents", "skills", "foo", "SKILL.md")))
		},
		"SKILL.md is symlink to in-repo .env": func(t *testing.T, root string) {
			writeRepoFile(t, root, ".env", secretSkill)
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills", "foo"), 0o750))
			advMust(t, os.Symlink("../../../.env", filepath.Join(root, ".agents", "skills", "foo", "SKILL.md")))
		},
		"SKILL.md is hard link to .env": func(t *testing.T, root string) {
			writeRepoFile(t, root, ".env", secretSkill)
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills", "foo"), 0o750))
			advMust(t, os.Link(filepath.Join(root, ".env"), filepath.Join(root, ".agents", "skills", "foo", "SKILL.md")))
		},
		"SKILL.md is hard link to outside file": func(t *testing.T, root string) {
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills", "foo"), 0o750))
			advMust(t, os.Link(filepath.Join(outside, "SKILL.md"), filepath.Join(root, ".agents", "skills", "foo", "SKILL.md")))
		},
		"SKILL.md is symlink to .git/config-like secret path": func(t *testing.T, root string) {
			writeRepoFile(t, root, ".aws/credentials", secretSkill)
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills", "foo"), 0o750))
			advMust(t, os.Symlink("../../../.aws/credentials", filepath.Join(root, ".agents", "skills", "foo", "SKILL.md")))
		},
		"skill dir is symlink to dir holding .env-hardlink": func(t *testing.T, root string) {
			writeRepoFile(t, root, "docs/x/SKILL.md", secretSkill)
			advMust(t, os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o750))
			advMust(t, os.Symlink("../../docs/x", filepath.Join(root, ".agents", "skills", "foo")))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSkillFixture(t)
			setup(t, f.root)
			res := advLoad(f, "foo")
			if strings.Contains(res.Text, advSecret) {
				t.Fatalf("SECRET LEAKED into tool result (IsError=%v): %q", res.IsError, res.Text)
			}
			t.Logf("IsError=%v text=%q", res.IsError, res.Text)
		})
	}
}

// TestAdvSymlinkInsideRepoToOtherSkillIsConsistent: a SKILL.md symlink to a
// regular file in the repo that is NOT under .agents/skills. Recorded for the
// report; the read gate judges the link's own rel path, not its target.
func TestAdvSymlinkInsideRepoToArbitraryFile(t *testing.T) {
	f := newSkillFixture(t)
	writeRepoFile(t, f.root, "docs/private-notes.md", "---\nname: foo\ndescription: d\n---\nNOTES\n")
	advMust(t, os.MkdirAll(filepath.Join(f.root, ".agents", "skills", "foo"), 0o750))
	advMust(t, os.Symlink("../../../docs/private-notes.md", filepath.Join(f.root, ".agents", "skills", "foo", "SKILL.md")))
	res := advLoad(f, "foo")
	t.Logf("in-repo symlink target: IsError=%v text=%q", res.IsError, res.Text)
}

// TestAdvFIFOSkillDoesNotHang: a FIFO named SKILL.md must be refused promptly,
// both by the tool and by the listing the prompt assembly runs every turn.
func TestAdvFIFOSkillDoesNotHang(t *testing.T) {
	f := newSkillFixture(t)
	dir := filepath.Join(f.root, ".agents", "skills", "foo")
	advMust(t, os.MkdirAll(dir, 0o750))
	if err := runMkfifo(filepath.Join(dir, "SKILL.md"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// A good sibling must still load: one hostile skill cannot poison the list.
	writeSkillFile(t, f.root, "good", fmt.Sprintf(advGoodSkill, "good"))

	done := make(chan harness.SkillSet, 1)
	go func() {
		set, _ := f.reg.LoadSkills(context.Background())
		done <- set
	}()
	select {
	case set := <-done:
		if _, ok := set.Find("good"); !ok {
			t.Errorf("a FIFO sibling hid the good skill: %+v", set)
		}
		reason, _ := set.Rejection("foo")
		t.Logf("fifo rejection: %s", reason)
	case <-time.After(5 * time.Second):
		t.Fatal("LoadSkills blocked on a FIFO SKILL.md (prompt assembly would hang every turn)")
	}
}

func TestAdvDirectoryAndDeviceNamedSKILLmd(t *testing.T) {
	f := newSkillFixture(t)
	advMust(t, os.MkdirAll(filepath.Join(f.root, ".agents", "skills", "foo", "SKILL.md"), 0o750))
	res := advLoad(f, "foo")
	if !res.IsError {
		t.Fatalf("directory SKILL.md produced a body: %q", res.Text)
	}
	advMust(t, os.MkdirAll(filepath.Join(f.root, ".agents", "skills", "dev"), 0o750))
	if err := os.Symlink("/dev/zero", filepath.Join(f.root, ".agents", "skills", "dev", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	done := make(chan tools.Result, 1)
	go func() { done <- advLoad(f, "dev") }()
	select {
	case r := <-done:
		if !r.IsError {
			t.Fatalf("/dev/zero symlink produced a body (len %d)", len(r.Text))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a /dev/zero symlink SKILL.md did not return")
	}
}

// TestAdvToctouSwapNeverLeaks flips SKILL.md between a regular file and a
// symlink to an outside secret while the tool runs.
func TestAdvToctouSwapNeverLeaks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "s.md")
	advMust(t, os.WriteFile(secret, []byte("---\nname: foo\ndescription: d\n---\n"+advSecret+"\n"), 0o600))
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "foo", fmt.Sprintf(advGoodSkill, "foo"))
	p := filepath.Join(f.root, ".agents", "skills", "foo", "SKILL.md")
	tmp := p + ".swap"

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = os.Remove(tmp)
			if os.Symlink(secret, tmp) == nil {
				_ = os.Rename(tmp, p)
			}
			_ = os.WriteFile(tmp, []byte(fmt.Sprintf(advGoodSkill, "foo")), 0o600)
			_ = os.Rename(tmp, p)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res := advLoad(f, "foo")
		if strings.Contains(res.Text, advSecret) {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("TOCTOU leak: %q", res.Text)
		}
	}
	stop.Store(true)
	wg.Wait()
}

// TestAdvReflectedNameIsBounded: an attacker-controlled (or model-hallucinated)
// 1 MB name must not be echoed back into model context.
func TestAdvReflectedNameIsBounded(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "foo", fmt.Sprintf(advGoodSkill, "foo"))
	big := strings.Repeat("A", 1<<20)
	res := advLoad(f, big)
	if !res.IsError {
		t.Fatal("1 MB name accepted")
	}
	if len(res.Text) > 4096 {
		t.Fatalf("refusal echoes %d bytes of the attacker-controlled name into the model context", len(res.Text))
	}
	// A valid-shaped long name goes through the not-found path instead.
	res = advLoad(f, strings.Repeat("a", 1<<20))
	if len(res.Text) > 4096 {
		t.Fatalf("not-found path echoes %d bytes of the name", len(res.Text))
	}
}

func TestAdvWrongTypedArguments(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "foo", fmt.Sprintf(advGoodSkill, "foo"))
	for _, raw := range []string{
		`{"name": 5}`, `{"name": null}`, `{}`, `{"name": ["foo"]}`, `{"name": {"x":1}}`,
		`null`, `[]`, `"foo"`, ``, `{"name":"foo","name":"bar"}`, `{"name":"foo"} trailing`,
	} {
		res := f.reg.loadSkill(context.Background(), tools.Call{ID: "c", Name: "devcouncil_load_skill", Arguments: json.RawMessage(raw)})
		t.Logf("%-32s IsError=%v %q", raw, res.IsError, trunc(res.Text, 90))
		if raw == `{"name": 5}` || raw == `{}` || raw == `{"name": null}` {
			if !res.IsError {
				t.Errorf("%s was accepted", raw)
			}
		}
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// TestAdvConcurrentLoads: 50 callers at once. Run under -race where cgo is
// available.
func TestAdvConcurrentLoads(t *testing.T) {
	f := newSkillFixture(t)
	for i := range 20 {
		n := fmt.Sprintf("s%02d", i)
		writeSkillFile(t, f.root, n, fmt.Sprintf(advGoodSkill, n))
	}
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := fmt.Sprintf("s%02d", i%20)
			res := advLoad(f, n)
			if res.IsError || res.Text != "BODY\n" {
				errs <- fmt.Sprintf("%s: %q", n, res.Text)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestAdvCancelledContext(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "foo", fmt.Sprintf(advGoodSkill, "foo"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]string{"name": "foo"})
	res := f.reg.loadSkill(ctx, tools.Call{ID: "c", Name: "devcouncil_load_skill", Arguments: raw})
	if !res.IsError {
		t.Fatalf("cancelled context still returned a body: %q", res.Text)
	}
	t.Logf("cancelled: %q", res.Text)
}

// TestAdvTenThousandSkillDirsCost: the listing runs every turn; bound its cost.
func TestAdvTenThousandSkillDirsCost(t *testing.T) {
	f := newSkillFixture(t)
	for i := range 10_000 {
		n := fmt.Sprintf("s%05d", i)
		advMust(t, os.MkdirAll(filepath.Join(f.root, ".agents", "skills", n), 0o750))
		if i < 300 {
			writeSkillFile(t, f.root, n, fmt.Sprintf(advGoodSkill, n))
		}
	}
	start := time.Now()
	set, err := f.reg.LoadSkills(context.Background())
	el := time.Since(start)
	advMust(t, err)
	t.Logf("10k dirs: %v; skills=%d rejected=%d", el, len(set.Skills), len(set.Rejected))
	// Every read goes through the full read gate, which re-lists .agents/skills
	// (10k entries here) once per read, so the cost is MaxSkills listings of the
	// big directory: seconds, and DevCouncil's to reduce. What this test holds is
	// that the work stays bounded by MaxSkills and not by the directory: the
	// bound below fails if all 10k are examined.
	if el > 20*time.Second {
		t.Errorf("listing 10k dirs took %v", el)
	}
	if set.NotExamined != 10_000-harness.MaxSkills {
		t.Errorf("NotExamined = %d, want %d: the cap did not bound the scan", set.NotExamined, 10_000-harness.MaxSkills)
	}
	if len(set.Rejected) > harness.MaxSkills {
		t.Errorf("Rejected holds %d entries for one listing: unexamined directories are materialised one by one instead of counted", len(set.Rejected))
	}
}

// TestAdvCaseInsensitiveFS records how a case-variant tree behaves on this
// filesystem.
func TestAdvCaseVariantTree(t *testing.T) {
	f := newSkillFixture(t)
	writeRepoFile(t, f.root, ".Agents/Skills/Foo/SKILL.md", "---\nname: Foo\ndescription: d\n---\nB\n")
	set, err := f.reg.LoadSkills(context.Background())
	t.Logf("case-variant: err=%v skills=%v rejected=%v", err, set.Skills, set.Rejected)
	res := advLoad(f, "foo")
	t.Logf("load foo: IsError=%v %q", res.IsError, res.Text)
	// On a case-insensitive FS, .Agents is .agents: the directory is seen and
	// refused as "Foo". The model asking for "foo" must be told Foo was seen.
	if len(set.Rejected) > 0 && !strings.Contains(res.Text, "Foo") {
		t.Errorf("a rejected case-variant directory is invisible to the load tool's answer: %q", res.Text)
	}
}

func TestAdvNonASCIIDirNames(t *testing.T) {
	f := newSkillFixture(t)
	for _, n := range []string{"café", "café", "con", "nul", "aux"} {
		writeSkillFile(t, f.root, n, fmt.Sprintf(advGoodSkill, n))
	}
	set, _ := f.reg.LoadSkills(context.Background())
	for _, s := range set.Skills {
		if !harness.ValidSkillName(s.Name) || strings.ContainsAny(s.Name, "é́") {
			t.Errorf("accepted %q", s.Name)
		}
	}
	t.Logf("accepted=%d rejected=%v", len(set.Skills), set.Rejected)
}
