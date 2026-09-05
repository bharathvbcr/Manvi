package devcouncil

import (
	"strings"
	"testing"
)

// The tool surface has to enforce the read rung, not merely have one available.
func TestReadFileRefusesCredentialPaths(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "DB_PASSWORD=hunter2\n")
	writeRepoFile(t, f.root, "src/calc.go", "package src\n\nfunc Calc() {}\n")

	res := f.call("devcouncil_read_file", map[string]string{"path": ".env"})
	if !res.IsError {
		t.Fatalf("reading .env must be refused, got: %q", res.Text)
	}
	if strings.Contains(res.Text, "hunter2") {
		t.Fatal("the refusal reproduced the credential it refused to hand over")
	}
	if !strings.Contains(res.Text, "path.secret_read") {
		t.Errorf("the refusal should be routable by rule, got %q", res.Text)
	}

	// Ordinary source still reads, so this is a gate and not a wall.
	if res := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"}); res.IsError {
		t.Fatalf("reading ordinary source must still work, got %q", res.Text)
	}
}

// grep reaches the same bytes read_file does. A gate on one and not the other
// is a gate with a documented way around it.
func TestGrepWithholdsCredentialFilesAndSaysSo(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "DB_PASSWORD=hunter2\n")
	writeRepoFile(t, f.root, "src/config.go", "package src\n\n// DB_PASSWORD is read from the environment.\n")

	payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": "DB_PASSWORD", "include_ignored": true,
	})
	raw, _ := payload["matches"].([]any)
	for _, m := range raw {
		entry, _ := m.(map[string]any)
		path, _ := entry["path"].(string)
		if strings.Contains(path, ".env") {
			t.Errorf("a match from %q reached the model", path)
		}
		if line, _ := entry["line"].(string); strings.Contains(line, "hunter2") {
			t.Error("a credential value reached the model through grep")
		}
	}

	// Withholding silently would be the other failure: a filtered result that
	// looks like a complete one.
	withheld, ok := payload["withheld"].(map[string]any)
	if !ok {
		t.Fatalf("matches were withheld and the result did not say so: %v", payload)
	}
	if note, _ := withheld["note"].(string); !strings.Contains(note, "does not cover") {
		t.Errorf("the note should say the result is incomplete, got %q", note)
	}
}
