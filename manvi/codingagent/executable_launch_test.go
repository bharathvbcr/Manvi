package codingagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shebang line other than sh must be refused before any spawn, with the
// marker the script would touch never created, and must say which interpreter.
func TestStartClaudeRefusesShellAndUnknownInterpretersBeforeAnySpawn(t *testing.T) {
	for _, tc := range []struct{ shebang, want string }{
		{"#!/bin/zsh", "shell wrapper (interpreter zsh)"},
		{"#!/usr/bin/env bash", "shell wrapper (interpreter bash)"},
		{"#!/usr/bin/env python3", "a script run by python3"},
		{"#!/usr/bin/perl", "a script run by perl"},
	} {
		t.Run(tc.shebang, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "ran")
			script := filepath.Join(dir, "claude")
			// #nosec G306 -- a test script that has to be executable, in a temp dir.
			if err := os.WriteFile(script, []byte(tc.shebang+"\ntouch '"+marker+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			s, err := StartClaude(t.Context(), Options{Program: script, Cwd: checkoutRoot(t), Mode: "ask"})
			if s != nil {
				t.Fatal("a session started")
			}
			requireRefusedBeforeStart(t, err, script, marker)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not say %q", err, tc.want)
			}
		})
	}
}

// An npm-style launcher is accepted by the launch-time check. Acceptance only:
// the fixture harness cannot make a node script speak the protocol, so that the
// kernel runs the interpreter in place under the same pid is not exercised here.
func TestAnNpmStyleNodeLauncherPassesTheLaunchCheck(t *testing.T) {
	script := writeFile(t, "claude", 0o755, []byte("#!/usr/bin/env node\nrequire('./cli.js')\n"))
	if err := requireLaunchableClaude(script); err != nil {
		t.Fatalf("an npm-style launcher was refused: %v", err)
	}
	var wrapper *ClaudeWrapperError
	zsh := writeFile(t, "claude", 0o755, []byte("#!/bin/zsh\n"))
	if err := requireLaunchableClaude(zsh); !errors.As(err, &wrapper) {
		t.Fatalf("a zsh wrapper was accepted: %v", err)
	}
}

func TestAForeignPidReportCarriesTheWrapperHintAndStaysRefused(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := startClaude(t.Context(), Options{Program: bin, Cwd: checkoutRoot(t), Mode: "ask"}, fixtureArgs("foreign-pid"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Close() })
	err = s.Initialize(t.Context())
	if err == nil || !strings.Contains(err.Error(), "process identity other than the one this host started") ||
		!strings.Contains(err.Error(), ClaudeBinaryVariable) || !strings.Contains(err.Error(), "wrapper") {
		t.Fatalf("identity mismatch lost its refusal or its hint: %v", err)
	}
}
