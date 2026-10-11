package computer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv/childenvtest"
)

// The desktop broker launches helpers of its own, which inherit what it was
// given. A stand-in broker records its environment and exits.
func TestTheBrokerDoesNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "manvi-desktop")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nenv > '"+dump+".tmp' && mv '"+dump+".tmp' '"+dump+"'\n"), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	c, err := Start(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		seen, err := os.ReadFile(dump) // #nosec G304 -- a file under this test's own t.TempDir
		if err == nil {
			childenvtest.Check(t, string(seen))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in broker did not record its environment: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
