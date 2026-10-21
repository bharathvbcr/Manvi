package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A repository can plant a FIFO (or any non-directory) at .agents/skills.
// Opening a FIFO blocks until a writer appears, so the listing refuses the path
// by its type, without opening it, and says why.
func plantSkillsPath(t *testing.T, mode string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, SkillsDir)
	switch mode {
	case "fifo":
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	case "file":
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func withinSecond(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s blocked for more than a second", what)
	}
}

func TestSpecialFileAtSkillsPathIsRefusedWithoutBeingOpened(t *testing.T) {
	for _, mode := range []string{"fifo", "file"} {
		t.Run(mode, func(t *testing.T) {
			root := plantSkillsPath(t, mode)
			withinSecond(t, "LoadSkills", func() {
				if _, err := LoadSkills(context.Background(), root, stubRead("")); err == nil {
					t.Error("LoadSkills accepted a non-directory as the skills directory")
				}
			})
			withinSecond(t, "LookupSkill", func() {
				if _, err := LookupSkill(context.Background(), root, stubRead(""), "anything"); err == nil {
					t.Error("LookupSkill accepted a non-directory as the skills directory")
				}
			})
		})
	}
}

func TestSymlinkToFIFOAsSkillsPathIsRefused(t *testing.T) {
	root := plantSkillsPath(t, "none")
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.Symlink("../pipe", filepath.Join(root, SkillsDir)); err != nil {
		t.Fatal(err)
	}
	withinSecond(t, "LoadSkills", func() {
		if _, err := LoadSkills(context.Background(), root, stubRead("")); err == nil {
			t.Error("a symlink to a FIFO was accepted as the skills directory")
		}
	})
}

func TestListingHonoursACancelledContext(t *testing.T) {
	root := plantSkillsPath(t, "none")
	if err := os.MkdirAll(filepath.Join(root, SkillsDir, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadSkills(ctx, root, stubRead("")); !errors.Is(err, context.Canceled) {
		t.Errorf("LoadSkills under a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := LookupSkill(ctx, root, stubRead(""), "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("LookupSkill under a cancelled context = %v, want context.Canceled", err)
	}
}
