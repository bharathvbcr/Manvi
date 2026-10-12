//go:build unix

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A server that closes its stdin and only afterwards explains itself on stderr
// fails the caller's write with EPIPE. That error used to be returned at once,
// without the explanation the server was about to give, so the result of the
// same failure depended on whether the child's stderr had been read yet: a
// "server reported" suffix when the write landed first, a bare "broken pipe"
// when the child had already gone. A credential in the explanation is what
// the harness scrubber redacts, so the same race decided whether the result
// carried one.
func TestWriteFailureCarriesWhatTheServerSaidAfterClosingStdin(t *testing.T) {
	c, err := NewClient(ServerConfig{
		Name:    "late",
		Command: "sh",
		Args:    []string{"-c", `exec 0<&-; sleep 0.3; echo "auth rejected for token ghp_LATE0123456789" >&2; exit 1`},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Let the child close its stdin so the write below is the one that breaks.
	time.Sleep(150 * time.Millisecond)

	err = c.Call(context.Background(), "initialize", nil, nil)
	if err == nil {
		t.Fatal("the call to a server with no stdin succeeded")
	}
	if !strings.Contains(err.Error(), "auth rejected for token ghp_LATE0123456789") {
		t.Fatalf("the failed call dropped the server's own explanation: %v", err)
	}
}
