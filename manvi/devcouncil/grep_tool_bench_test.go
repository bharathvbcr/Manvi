package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func benchPut(tb testing.TB, root, rel, body string) {
	tb.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkGrepTool measures the grep tool end to end through the registry, over
// the 3,000-file shape benchRepo uses (40 package directories). The sub-cases
// are the ones the withheld-file partitioning changes the cost of: nothing
// refused, a refused .env at the root, and a refused file deep in a directory
// with siblings, which makes each sibling its own search.
func BenchmarkGrepTool(b *testing.B) {
	seed := func(f *fixture, extra func()) {
		for i := range 3000 {
			benchPut(b, f.root, fmt.Sprintf("pkg%d/file%d.go", i%40, i),
				fmt.Sprintf("package p\n\nfunc F%d() int { return %d }\n", i, i))
		}
		if extra != nil {
			extra()
		}
	}
	run := func(b *testing.B, f *fixture, args map[string]any) {
		b.Helper()
		b.ResetTimer()
		for range b.N {
			if res := f.call("devcouncil_grep", args); res.IsError {
				b.Fatalf("grep failed: %s", res.Text)
			}
		}
	}
	pattern := map[string]any{"pattern": `func F1234\b`}
	withIgnored := map[string]any{"pattern": `func F1234\b`, "include_ignored": true}

	b.Run("nothing_refused", func(b *testing.B) {
		f := newFixture(b)
		seed(f, nil)
		run(b, f, pattern)
	})
	b.Run("root_env_refused", func(b *testing.B) {
		f := newFixture(b)
		seed(f, func() { benchPut(b, f.root, ".env", "TOKEN=abc\n") })
		run(b, f, withIgnored)
	})
	b.Run("deep_file_refused", func(b *testing.B) {
		f := newFixture(b)
		seed(f, func() { benchPut(b, f.root, "pkg7/sub/deep/.env", "TOKEN=abc\n") })
		run(b, f, withIgnored)
	})
	b.Run("hundred_targets", func(b *testing.B) {
		f := newFixture(b)
		seed(f, func() {
			for i := range 100 {
				benchPut(b, f.root, fmt.Sprintf("flat/f%d.txt", i), "x\n")
			}
			benchPut(b, f.root, "flat/.env", "TOKEN=abc\n")
		})
		run(b, f, map[string]any{"pattern": `func F1234\b`, "path": "flat", "include_ignored": true})
	})
}
