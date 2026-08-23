package serve

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestServeHammerOversizedLinesThenCancel: ten oversized frames in a row must
// each be refused without ending the session, and cancellation must still end
// it promptly afterwards.
func TestServeHammerOversizedLinesThenCancel(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	var out synchronizedBuffer
	srv := New(&out, hostOpts())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, pr) }()

	big := strings.Repeat("x", 9<<20) // 9 MiB, over the 8 MiB line cap
	for i := 0; i < 10; i++ {
		if _, err := pw.WriteString("{\"id\":\"big" + string(rune('0'+i)) + "\",\"op\":\"x\",\"params\":{\"" + big + "\":1}}\n"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && strings.Count(out.String(), "E_TOO_LARGE") < 10 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Count(out.String(), "E_TOO_LARGE"); got < 10 {
		t.Fatalf("only %d of 10 oversized frames were refused before timeout", got)
	}
	cancel()
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not end the hammered server")
	}
}
