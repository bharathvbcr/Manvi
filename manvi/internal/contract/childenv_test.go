package contract

import (
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
