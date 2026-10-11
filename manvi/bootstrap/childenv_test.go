package bootstrap

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv/childenvtest"
)

// A repository's alias, hook or filter runs with the git child's environment.
// An alias that prints it is that vector, run through the real seam.
func TestGitChildrenDoNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	out, err := git(context.Background(), t.TempDir(), "-c", "alias.dumpenv=!env", "dumpenv")
	if err != nil {
		t.Fatal(err)
	}
	childenvtest.Check(t, string(out))
}
