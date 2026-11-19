//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// cmd/manvi hands the registry core.artifacts, which is nil when
// newArtifactStore refused the configured state directory (services.go discards
// that error). The registry then builds its own store at
// <root>/.devcouncil/artifacts, whatever MANVI_STATE_DIR says: a refusal turns
// into a write to the default location instead of an error. Defect: the
// fallback must not make a writable store when the configured one was refused.
func TestZadvNilArtifactsDepDoesNotFallBackToTheDefaultLocationDefect(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "state")
	t.Setenv("MANVI_STATE_DIR", elsewhere)
	r := &Registry{deps: Deps{Root: root}}
	store, err := r.getArtifactStore()
	if err == nil {
		_, err = store.Create("fallback.md", "x", artifacts.Metadata{})
	}
	if _, statErr := os.Stat(filepath.Join(root, ".devcouncil", "artifacts", "fallback.md")); statErr == nil {
		t.Fatalf("with no store handed in and MANVI_STATE_DIR=%s, create_artifact wrote to %s/.devcouncil/artifacts (Create err: %v)", elsewhere, root, err)
	}
}
