package main

import (
	"context"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv/childenvtest"
)

// Quick actions run git pull, git push and gh in the operator's repository: a
// post-merge hook runs with the same environment. They keep everything the
// operator's tools need except the provider credentials.
func TestQuickActionsDoNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	result, err := osQuickCommandRunner{}.Run(context.Background(), 5*time.Second, t.TempDir(), "env")
	if err != nil {
		t.Fatal(err)
	}
	childenvtest.Check(t, result.Output)
}
