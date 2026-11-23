package devcouncil

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// failingDepsDevmap is a devmap whose index is healthy but whose `deps` fails
// for one file, the way a per-file query fails on a path the index cannot
// resolve.
func failingDepsDevmap(t *testing.T, failFile string) *Registry {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --help \"*) printf '      --graph-output <PATH>\\n'; exit 0;; esac\n" +
		"case \"$2\" in\n" +
		"  status) echo '" + stubHealthy + "';;\n" +
		"  deps) for last; do :; done\n" +
		"        if [ \"$last\" = '" + failFile + "' ]; then echo 'no such file in the index' >&2; exit 3; fi\n" +
		"        printf '{\"items\":[{\"source_file\":\"%s\",\"target_file\":\"dep-of-%s\"}],\"hidden\":0}\\n' \"$last\" \"$last\";;\n" +
		"  *) echo '{\"items\":[],\"hidden\":0}';;\n" +
		"esac\n"
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test stub the test must be able to execute
		t.Fatal(err)
	}
	return &Registry{deps: Deps{Root: dir, Map: devmap.New(path, dir)}, session: &Session{}}
}

// A file the graph could not answer for is a gap in the answer. The per-file
// error rode inside the file's own entry and nothing else said so, so the
// envelope claimed a complete answer: answer_is_partial was absent.
func TestGraphContextAFailedFileMakesTheAnswerPartial(t *testing.T) {
	reg := failingDepsDevmap(t, "pkg/bad.go")
	res := reg.graphContext(context.Background(), callOf(t, map[string]any{"files": []string{"pkg/ok.go", "pkg/bad.go"}}))
	if res.IsError {
		t.Fatalf("graph_context failed outright: %s", res.Text)
	}
	var body struct {
		Files []struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		} `json:"files"`
		Partial  bool     `json:"answer_is_partial"`
		Degraded []string `json:"degraded"`
	}
	if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
		t.Fatalf("unreadable answer: %v\n%s", err, res.Text)
	}
	if len(body.Files) != 2 || body.Files[1].Error == "" {
		t.Fatalf("the failing file did not carry its error: %s", res.Text)
	}
	if !body.Partial || len(body.Degraded) == 0 || len(res.Degraded) == 0 {
		t.Fatalf("one failed file left the answer claiming to be complete: %s", res.Text)
	}

	clean := failingDepsDevmap(t, "pkg/none.go")
	ok := clean.graphContext(context.Background(), callOf(t, map[string]any{"files": []string{"pkg/ok.go"}}))
	if ok.IsError || len(ok.Degraded) != 0 {
		t.Fatalf("a fully answered call was reported partial: %+v", ok)
	}
}
