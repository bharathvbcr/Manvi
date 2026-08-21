package main

import (
	"os"
	"path/filepath"
)

// readSource reads a file from the repository root, so a test can assert on
// how a call site is wired rather than on behaviour that would need a live
// provider to observe.
func readSource(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
