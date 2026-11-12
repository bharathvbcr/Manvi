package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file that is ref-shaped but cannot be read in full is not skipped: the
// command is refused before it runs.
func TestW21OversizedRefSourceFileRefusesTheCommand(t *testing.T) {
	f := shellCommitFixture(t, false)
	body := headOf(t, f.root) + "\n" + strings.Repeat("x", maxRootFileBytes+1)
	if err := os.WriteFile(filepath.Join(gitCommon(t, f.root), "BIG_HEAD"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := shell(f, "echo should-not-run > ran.txt")
	if !res.IsError || !strings.Contains(res.Text, "ref snapshot") {
		t.Fatalf("a command ran beside an unreadable ref source: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.root, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
}

// The parser: what git reads as a ref, and what it does not.
func TestW21RootFileParsing(t *testing.T) {
	oid := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	null := strings.Repeat("0", 40)
	for _, tc := range []struct {
		name, in string
		hashLen  int
		want     int
	}{
		{"single", oid + "\n", 40, 1},
		{"no newline", oid, 40, 1},
		{"trailing words", oid + "\t\tbranch 'x' of y\n", 40, 1},
		{"NUL after the id", oid + "\x00junk", 40, 1},
		{"sha256 repo", sha256 + "\n", 64, 1},
		{"sha256 id in a sha1 repo", sha256 + "\n", 40, 0},
		{"sha256sum line in a sha1 repo", sha256 + "  app.js\n", 40, 0},
		{"sha1 id in a sha256 repo", oid + "\n", 64, 0},
		{"multi", oid + "\n" + oid + "\t\tx\n", 40, 2},
		{"symbolic", "ref: refs/heads/main\n", 40, 0},
		{"config", "[core]\n\tbare = false\n", 40, 0},
		{"message", "fix the thing\n" + oid + "\n", 40, 0},
		{"leading space", " " + oid + "\n", 40, 0},
		{"null id", null + "\n", 40, 0},
		{"glued", oid + "x\n", 40, 0},
		{"41 hex", oid + "a\n", 40, 0},
		{"later junk is skipped", oid + "\nnot an id\n" + oid + "\n", 40, 2},
	} {
		if got := len(parseRootFile(tc.in, tc.hashLen)); got != tc.want {
			t.Errorf("%s: %d ids, want %d", tc.name, got, tc.want)
		}
	}
}
