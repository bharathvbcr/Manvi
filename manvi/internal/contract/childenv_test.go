package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedEnvironReads excuse an os.Environ() read that hands nothing to a
// child. As with the other allowlists here, an entry that no longer matches a
// read fails the test.
var allowedEnvironReads = map[string]string{
	"cmd/manvi/main.go": "flags.StaleEnv and flags.RetiredEnv read the variable names to report renamed and retired settings; nothing is spawned with them",
}

// TestNoChildInheritsThisProcessEnvironment is what `manvi version --json`
// means by withholds_provider_credentials: every child this harness starts is
// given an environment childenv built, so no spawn site can quietly fall back
// to inheriting the provider keys a host put here for the harness's own
// adapter.
func TestNoChildInheritsThisProcessEnvironment(t *testing.T) {
	m, err := Load(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	scan := m.ChildEnvSites(allowedEnvironReads)
	// The check on the check: the harness starts git, coding agents, the
	// desktop broker, MCP servers, verifiers and quick actions. A scan that
	// found fewer spawns than that is not looking at the harness.
	if scan.Spawns < 8 {
		t.Fatalf("the scan found only %d spawn sites; it is examining almost nothing", scan.Spawns)
	}
	var b strings.Builder
	for _, f := range scan.Findings {
		b.WriteString("  " + f.String() + "\n")
	}
	if b.Len() > 0 {
		t.Fatalf("children that would inherit this process's environment:\n\n%s", b.String())
	}
	for file, reason := range allowedEnvironReads {
		if !scan.Environ[file] {
			t.Errorf("%s is excused (%q) but no longer reads os.Environ(); delete the entry", file, reason)
		}
	}
}

// TestChildEnvBindsEnvToTheSpawnedVariable is the check on the check: an Env
// assignment excuses only the command it is assigned to. Two exec.Command
// calls and one `cmd.Env =` leave the second child inheriting everything, so
// exactly one finding must name it.
func TestChildEnvBindsEnvToTheSpawnedVariable(t *testing.T) {
	const src = `package fixture

import "os/exec"

func two(name string) error {
	a := exec.Command(name)
	b := exec.Command(name)
	a.Env = []string{"PATH=/usr/bin"}
	if err := a.Run(); err != nil {
		return err
	}
	return b.Run()
}

func literal(name string) error {
	c := &exec.Cmd{Path: name, Env: []string{}}
	return c.Run()
}

func viaField(name string) error {
	var h struct{ cmd *exec.Cmd }
	h.cmd = exec.Command(name)
	h.cmd.Env = []string{}
	return h.cmd.Run()
}

func unbound(name string) error {
	return exec.Command(name).Run()
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	scan := m.ChildEnvSites(nil)
	if scan.Spawns != 5 {
		t.Fatalf("examined %d spawns, want 5", scan.Spawns)
	}
	got := map[string]int{}
	for _, f := range scan.Findings {
		got[f.Name]++
	}
	// two() leaves b without an Env; unbound() has no variable to assign one to.
	if len(scan.Findings) != 2 || got["two"] != 1 || got["unbound"] != 1 {
		t.Fatalf("findings = %v, want one in two() and one in unbound()", scan.Findings)
	}
}
