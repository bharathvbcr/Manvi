package devcouncil

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The oids a ref guard call excludes grow with the refs a repository holds, so
// they go to rev-list on its standard input: on the command line, tens of
// thousands of them exceed the operating system's argument limit and the call
// fails with "argument list too long".
func TestNewCommitsWithManyRecordedOIDsDoesNotExceedTheArgumentLimit(t *testing.T) {
	f := shellCommitFixture(t, false)
	const n = 30000
	var in strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&in, "commit refs/heads/chain\nmark :%d\ncommitter t <t@example.com> 1700000000 +0000\ndata %d\n%d\n", i, len(fmt.Sprint(i)), i)
		if i > 1 {
			fmt.Fprintf(&in, "from :%d\n", i-1)
		}
		in.WriteString("\n")
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", f.root, "fast-import", "--quiet") // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	recorded := strings.Fields(gitOut(t, f.root, "rev-list", "refs/heads/chain"))
	if len(recorded) != n {
		t.Fatalf("built %d recorded oids, want %d", len(recorded), n)
	}
	head := headOf(t, f.root)
	got, err := f.reg.newCommits(t.Context(), head, recorded)
	if err != nil {
		t.Fatalf("newCommits with %d recorded oids: %v", n, err)
	}
	if !slices.Equal(got, []string{head}) {
		t.Errorf("newCommits = %v, want only %s", got, head)
	}
}
