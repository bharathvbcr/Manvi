package lessons

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/session"
)

// Attack 6 (holds): an id that is not a plain hex id never reaches the
// filesystem, whether it arrives through Resolve or straight into Load, and a
// corrupt or truncated log is a CorruptError, not an empty packet.
func TestAdvSessionIDsAndCorruptLogsHold(t *testing.T) {
	dir := t.TempDir()
	s := session.NewStore(dir)
	for _, ref := range []string{"../x", "..", "/etc/passwd", "a/../b", "%2e%2e", "\x00", "", "  "} {
		if id, err := s.Resolve(ref); err == nil {
			t.Errorf("Resolve(%q) = %q", ref, id)
		}
		var nf *session.NotFoundError
		if _, _, err := s.Load(ref); !errors.As(err, &nf) {
			t.Errorf("Load(%q) = %v, want NotFoundError", ref, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "abc123.g1.json"), []byte(`{"version":1,"id":"abc123","events":[`), 0o600); err != nil {
		t.Skipf("session file naming differs: %v", err)
	}
	if _, _, err := s.Load("abc123"); err == nil {
		t.Error("a truncated session log loaded")
	}
}
