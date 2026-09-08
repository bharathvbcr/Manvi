package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGraphPathExtractorAcceptsCompactAndPrettyJSONAndOnlyFiles(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	script := filepath.Join(root, "scripts", "graph-file-paths.py")
	fixtures := []string{
		`{"nodes":[{"kind":"file","path":"b.go"},{"kind":"package","path":"package:fake"},{"kind":"file","path":"a.go"}]}`,
		"{\n  \"nodes\": [\n    {\"kind\": \"file\", \"path\": \"b.go\"},\n    {\"kind\": \"package\", \"path\": \"package:fake\"},\n    {\"kind\": \"file\", \"path\": \"a.go\"}\n  ]\n}",
	}
	for i, fixture := range fixtures {
		path := filepath.Join(t.TempDir(), "graph.json")
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("python3", script, path).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture %d: %v: %s", i, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != "a.go\nb.go" {
			t.Fatalf("fixture %d paths = %q", i, got)
		}
	}
}
