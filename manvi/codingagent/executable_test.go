package codingagent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wrapperScript writes an executable shell script that records being run, so a
// test can prove the check never executed the candidate.
func wrapperScript(t *testing.T, dir, name string) (path, ranMarker string) {
	t.Helper()
	ranMarker = filepath.Join(t.TempDir(), "ran")
	path = filepath.Join(dir, name)
	body := "#!/bin/sh\ntouch '" + ranMarker + "'\nexec /bin/echo '1.0.0 (Claude Code)'\n"
	// #nosec G306 -- a test wrapper that has to be executable, in a temp dir.
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, ranMarker
}

func checkoutRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func requireRefusedBeforeStart(t *testing.T, err error, wantPath, ranMarker string) {
	t.Helper()
	var refusal *ClaudeWrapperError
	if !errors.As(err, &refusal) {
		t.Fatalf("want *ClaudeWrapperError, got %v", err)
	}
	if !NoProcessStarted(err) {
		t.Fatalf("refusal was not certified as before any process: %v", err)
	}
	for _, want := range []string{ClaudeBinaryVariable, wantPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	if _, statErr := os.Stat(ranMarker); statErr == nil {
		t.Fatal("the candidate was executed by the check")
	}
}

func TestStartClaudeRefusesAWrapperScriptFoundOnPATH(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script, marker := wrapperScript(t, dir, "claude")
	t.Setenv("PATH", dir)
	s, err := StartClaude(t.Context(), Options{Program: "claude", Cwd: checkoutRoot(t), Mode: "ask"})
	if s != nil {
		t.Fatal("a session started from a wrapper script")
	}
	requireRefusedBeforeStart(t, err, script, marker)
}

func TestStartClaudeRefusesAScriptNamedByTheConfiguredPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script, marker := wrapperScript(t, dir, "my-claude")
	s, err := StartClaude(t.Context(), Options{Program: script, Cwd: checkoutRoot(t), Mode: "ask"})
	if s != nil {
		t.Fatal("a session started from a script")
	}
	requireRefusedBeforeStart(t, err, script, marker)
}

func TestStartClaudeNamesWhereASymlinkedScriptLeads(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script, marker := wrapperScript(t, dir, "real-script")
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(script, link); err != nil {
		t.Fatal(err)
	}
	_, err = StartClaude(t.Context(), Options{Program: link, Cwd: checkoutRoot(t), Mode: "ask"})
	requireRefusedBeforeStart(t, err, script, marker)
	if !strings.Contains(err.Error(), link) {
		t.Fatalf("refusal %q does not name the configured path %q", err, link)
	}
}

func TestAMissingClaudeBinaryKeepsItsExistingError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := StartClaude(t.Context(), Options{Program: missing, Cwd: checkoutRoot(t), Mode: "ask"})
	var refusal *ClaudeWrapperError
	if err == nil || errors.As(err, &refusal) || !NoProcessStarted(err) || !strings.Contains(err.Error(), "resolve managed Claude executable") {
		t.Fatalf("a missing binary changed its error: %v", err)
	}
}

func TestANativeExecutableAndASymlinkToOneAreAccepted(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireLaunchableClaude(bin); err != nil {
		t.Fatalf("the test binary itself was refused: %v", err)
	}
	link := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if err := requireLaunchableClaude(link); err != nil {
		t.Fatalf("a symlink to a native executable was refused: %v", err)
	}
}

func writeFile(t *testing.T, name string, mode os.FileMode, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	// #nosec G306 -- the mode is the variable under test, in a temp dir.
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequireLaunchableClaudeByHeader(t *testing.T) {
	javaClass := []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x41}
	universal := []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x02, 0x01}
	for _, tc := range []struct {
		name    string
		content []byte
		accept  bool
	}{
		{"elf", []byte("\x7fELF\x02\x01\x01\x00"), true},
		{"macho64-le", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01}, true},
		{"macho32-be", []byte{0xfe, 0xed, 0xfa, 0xce, 0, 0, 0, 0}, true},
		{"macho-universal", universal, true},
		{"pe", []byte("MZ\x90\x00\x03\x00\x00\x00"), true},
		{"java-class-shares-the-universal-magic", javaClass, false},
		{"truncated-universal", []byte{0xca, 0xfe, 0xba, 0xbe}, false},
		{"sh-script", []byte("#!/bin/sh\necho\n"), false},
		{"npm-env-node-script", []byte("#!/usr/bin/env node\nimport './cli.js'\n"), true},
		{"env-S-node-flags", []byte("#!/usr/bin/env -S node --no-warnings --max-old-space-size=4096\n"), true},
		{"direct-node-path", []byte("#!/usr/local/bin/node\n"), true},
		{"bun-and-deno", []byte("#!/usr/bin/env bun\n"), true},
		{"deno-direct", []byte("#!/opt/homebrew/bin/deno run\n"), true},
		{"crlf-node", []byte("#!/usr/bin/env node\r\n"), true},
		{"zsh-direct", []byte("#!/bin/zsh\nexec real\n"), false},
		{"env-bash", []byte("#!/usr/bin/env bash\n"), false},
		{"env-S-sh", []byte("#!/usr/bin/env -S sh -e\n"), false},
		{"env-with-assignment-then-zsh", []byte("#!/usr/bin/env FOO=1 zsh\n"), false},
		{"env-python", []byte("#!/usr/bin/env python3\n"), false},
		{"bare-env", []byte("#!/usr/bin/env\n"), false},
		{"empty-shebang", []byte("#!\n"), false},
		{"unterminated-long-line", append([]byte("#!/usr/bin/env node "), bytes.Repeat([]byte("x"), 400)...), false},
		{"bom-before-shebang", []byte("\xef\xbb\xbf#!/bin/sh\n"), false},
		{"bom-before-node-shebang", []byte("\xef\xbb\xbf#!/usr/bin/env node\n"), false},
		{"leading-space-shebang", []byte(" #!/bin/sh\n"), false},
		{"zero-bytes", nil, false},
		{"one-byte", []byte("M"), false},
		{"text", []byte("not a program"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireLaunchableClaude(writeFile(t, "claude", 0o700, tc.content))
			var refusal *ClaudeWrapperError
			switch {
			case tc.accept && err != nil:
				t.Fatalf("a native header was refused: %v", err)
			case !tc.accept && !errors.As(err, &refusal):
				t.Fatalf("a non-native file was not refused with the typed error: %v", err)
			}
		})
	}
}

func TestRequireLaunchableClaudeRefusesWhatIsNotAnExecutableRegularFile(t *testing.T) {
	var refusal *ClaudeWrapperError
	if err := requireLaunchableClaude(t.TempDir()); !errors.As(err, &refusal) {
		t.Fatalf("a directory was not refused: %v", err)
	}
	if runtime.GOOS != "windows" {
		plain := writeFile(t, "claude", 0o600, []byte("\x7fELF\x02\x01\x01\x00"))
		err := requireLaunchableClaude(plain)
		if !errors.As(err, &refusal) {
			t.Fatalf("a non-executable file was not refused: %v", err)
		}
		if !strings.Contains(refusal.Reason, "not marked executable") {
			t.Fatalf("wrong reason: %q", refusal.Reason)
		}
	}
}

func TestRequireLaunchableClaudeReadsABoundedPrefixOfAHugeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	// #nosec G302,G304 -- a sparse test file in a temp dir that must be executable.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 30); err != nil {
		t.Skipf("the filesystem will not make a sparse 8 GiB file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var refusal *ClaudeWrapperError
	if err := requireLaunchableClaude(path); !errors.As(err, &refusal) {
		t.Fatalf("a huge script was not refused: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the check took %v on a huge file; the read is not bounded", elapsed)
	}
}
