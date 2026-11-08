package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExecFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil { //nolint:gosec // the file must be executable to stand in for a tool; it lives under t.TempDir
		t.Fatal(err)
	}
}

func fakeMachO() []byte {
	// 64-bit Mach-O magic (0xfeedfacf) accepted by codingagent.CheckLaunchableClaude.
	return []byte{0xfe, 0xed, 0xfa, 0xcf, 0x00, 0x00, 0x00, 0x00}
}

func claudeTestName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

func TestToolBinarySkipsShellWrapperForLaunchableClaudeOnLaterPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang wrapper test")
	}
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	homeDir := t.TempDir()

	wrapper := filepath.Join(firstDir, claudeTestName())
	realBin := filepath.Join(secondDir, claudeTestName())
	writeExecFile(t, wrapper, []byte("#!/bin/sh\nexec real-claude \"$@\"\n"))
	writeExecFile(t, realBin, fakeMachO())

	t.Setenv("MANVI_CLAUDE_BINARY", "")
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PATH", firstDir+string(filepath.ListSeparator)+secondDir)

	got := toolBinary("MANVI_CLAUDE_BINARY", "claude")
	if got != realBin {
		t.Fatalf("toolBinary chose %q, want launchable binary %q past shell wrapper %q", got, realBin, wrapper)
	}
}

func TestToolBinaryFallsBackToHighestSemverNativeClaudeInstallPastWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang wrapper test")
	}
	pathDir := t.TempDir()
	homeDir := t.TempDir()

	wrapper := filepath.Join(pathDir, claudeTestName())
	writeExecFile(t, wrapper, []byte("#!/usr/bin/env bash\nexec ~/.claude-bin/claude \"$@\"\n"))

	versionsDir := filepath.Join(homeDir, ".local", "share", "claude", "versions")
	writeExecFile(t, filepath.Join(versionsDir, "2.1.90"), fakeMachO())
	want := filepath.Join(versionsDir, "2.1.319")
	writeExecFile(t, want, fakeMachO())
	// Adversarial entries: a higher semver that is itself a shell wrapper, a directory, and a non-semver file.
	writeExecFile(t, filepath.Join(versionsDir, "9.9.9"), []byte("#!/bin/sh\necho fake\n"))
	writeExecFile(t, filepath.Join(versionsDir, "latest"), fakeMachO())
	if err := os.MkdirAll(filepath.Join(versionsDir, "99.0.0"), 0o750); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MANVI_CLAUDE_BINARY", "")
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PATH", pathDir)

	got := toolBinary("MANVI_CLAUDE_BINARY", "claude")
	if got != want {
		t.Fatalf("toolBinary chose %q, want highest launchable version %q", got, want)
	}
}

func TestToolBinaryNeverOverridesExplicitClaudeEnvOrHidesWrapperWhenAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang wrapper test")
	}
	pathDir := t.TempDir()
	homeDir := t.TempDir()

	wrapper := filepath.Join(pathDir, claudeTestName())
	writeExecFile(t, wrapper, []byte("#!/bin/sh\necho wrapper\n"))

	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PATH", pathDir)

	// When no launchable binary exists anywhere, return the wrapper path so StartClaude
	// reports the typed ClaudeWrapperError instead of a misleading "not found".
	t.Setenv("MANVI_CLAUDE_BINARY", "")
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != wrapper {
		t.Fatalf("toolBinary with only wrapper = %q, want %q", got, wrapper)
	}

	// Explicit MANVI_CLAUDE_BINARY always wins, even when a native install exists.
	writeExecFile(t, filepath.Join(homeDir, ".local", "share", "claude", "versions", "2.1.319"), fakeMachO())
	t.Setenv("MANVI_CLAUDE_BINARY", wrapper)
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != wrapper {
		t.Fatalf("explicit MANVI_CLAUDE_BINARY overridden: got %q, want %q", got, wrapper)
	}
}

func agyTestName() string {
	if runtime.GOOS == "windows" {
		return "agy.exe"
	}
	return "agy"
}

func TestToolBinaryResolvesLocalBinAgyPastWrapperAndUnderMinimalGUIPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang wrapper test")
	}
	pathDir := t.TempDir()
	homeDir := t.TempDir()

	// PATH carries an Electron shell wrapper (like /Applications/Antigravity.app/.../bin/agy),
	// while `agy install` placed the native Mach-O binary at ~/.local/bin/agy.
	wrapper := filepath.Join(pathDir, agyTestName())
	writeExecFile(t, wrapper, []byte("#!/usr/bin/env sh\nexec electron-agy \"$@\"\n"))
	localBinAgy := filepath.Join(homeDir, ".local", "bin", agyTestName())
	writeExecFile(t, localBinAgy, fakeMachO())

	t.Setenv("MANVI_AGY_BINARY", "")
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", pathDir)

	if got := toolBinary("MANVI_AGY_BINARY", "agy"); got != localBinAgy {
		t.Fatalf("toolBinary chose %q, want ~/.local/bin/agy %q past wrapper %q", got, localBinAgy, wrapper)
	}

	// Minimal macOS GUI PATH (no agy on PATH at all) still resolves ~/.local/bin/agy.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	if got := toolBinary("MANVI_AGY_BINARY", "agy"); got != localBinAgy {
		t.Fatalf("toolBinary under minimal GUI PATH chose %q, want %q", got, localBinAgy)
	}

	// Explicit MANVI_AGY_BINARY is never overridden.
	t.Setenv("MANVI_AGY_BINARY", wrapper)
	if got := toolBinary("MANVI_AGY_BINARY", "agy"); got != wrapper {
		t.Fatalf("explicit MANVI_AGY_BINARY overridden: got %q, want %q", got, wrapper)
	}
}
