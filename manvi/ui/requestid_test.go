package ui

import (
	"strings"
	"sync"
	"testing"
)

func TestNewRequestIDIsUniqueAndNamesItsKind(t *testing.T) {
	const workers, each = 8, 200
	var mu sync.Mutex
	seen := make(map[string]bool, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id := NewRequestID("approval")
				mu.Lock()
				if seen[id] {
					t.Errorf("id %q was issued twice", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*each {
		t.Fatalf("%d distinct ids, want %d", len(seen), workers*each)
	}
	id := NewRequestID("question")
	if !strings.HasPrefix(id, "question-") || strings.ContainsAny(id, " \t\n\"") {
		t.Fatalf("id %q is not a single token led by its kind", id)
	}
	if !strings.Contains(id, requestNonce) {
		t.Fatalf("id %q does not carry the per-process nonce %q", id, requestNonce)
	}
}
