package codingagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openClaudeSeatFixture(t *testing.T, cwd string) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(t.TempDir(), "seat-env")
	t.Setenv(envDumpVar, prefix)
	s, err := startClaude(t.Context(), Options{
		Program: bin,
		Cwd:     cwd,
		Mode:    "ask",
	}, fixtureArgs("normal"))
	if err != nil {
		t.Fatalf("startClaude failed: %v", err)
	}
	t.Cleanup(func() { _, _ = s.Close() })
	if err := s.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	seen, err := os.ReadFile(prefix + ".session") // #nosec G304 -- test temp file
	if err != nil {
		t.Fatalf("read session env dump: %v", err)
	}
	return string(seen)
}

func TestClaudeSeatRoutingResolvesPinnedRepoAndWorktreeAndFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	defaultClaude := filepath.Join(home, ".claude")
	seatChandra := filepath.Join(home, ".claude-chandra")
	seatLappi := filepath.Join(home, ".claude-lappi")
	for _, dir := range []string{defaultClaude, seatChandra, seatLappi} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	accountsFile := filepath.Join(home, ".claude-accounts")
	accountsContent := "# Registered Claude seats\nchandra ~/.claude-chandra\nlappi " + seatLappi + "\n"
	if err := os.WriteFile(accountsFile, []byte(accountsContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Pinned repository overrides stale ambient CLAUDE_CONFIG_DIR/CLAUDE_CODE_OAUTH_TOKEN
	// and sets BOTH CLAUDE_CONFIG_DIR and CLAUDE_SECURESTORAGE_CONFIG_DIR.
	repoPinned := filepath.Join(home, "Code", "GitPulse")
	subDir := filepath.Join(repoPinned, "src-tauri")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPinned, ".claude-account"), []byte("chandra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", seatLappi)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", seatLappi)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "stale-other-seat-token")

	resolvedChandra, err := filepath.EvalSymlinks(seatChandra)
	if err != nil {
		t.Fatal(err)
	}
	dump := openClaudeSeatFixture(t, subDir)
	if !strings.Contains(dump, "CLAUDE_CONFIG_DIR="+resolvedChandra) {
		t.Fatalf("pinned repo missing CLAUDE_CONFIG_DIR=%s:\n%s", resolvedChandra, dump)
	}
	if !strings.Contains(dump, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+resolvedChandra) {
		t.Fatalf("pinned repo missing CLAUDE_SECURESTORAGE_CONFIG_DIR=%s:\n%s", resolvedChandra, dump)
	}
	if strings.Contains(dump, "CLAUDE_CODE_OAUTH_TOKEN=") {
		t.Fatalf("pinned repo leaked stale CLAUDE_CODE_OAUTH_TOKEN into seat:\n%s", dump)
	}

	// 2. Linked git worktree outside the main checkout inherits the main checkout's .claude-account.
	wtGitAdmin := filepath.Join(repoPinned, ".git", "worktrees", "lane-1")
	if err := os.MkdirAll(wtGitAdmin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGitAdmin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalWorktree := filepath.Join(home, "Worktrees", "lane-1")
	if err := os.MkdirAll(externalWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalWorktree, ".git"), []byte("gitdir: "+wtGitAdmin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wtDump := openClaudeSeatFixture(t, externalWorktree)
	if !strings.Contains(wtDump, "CLAUDE_CONFIG_DIR="+resolvedChandra) || !strings.Contains(wtDump, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+resolvedChandra) {
		t.Fatalf("linked external worktree did not inherit main repo seat %s:\n%s", resolvedChandra, wtDump)
	}

	// 3. Unpinned repo does not inherit a leaked pinned-seat CLAUDE_CONFIG_DIR from the parent shell.
	unpinnedRepo := filepath.Join(home, "Code", "Graft")
	if err := os.MkdirAll(unpinnedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	unpinnedDump := openClaudeSeatFixture(t, unpinnedRepo)
	if strings.Contains(unpinnedDump, "CLAUDE_CONFIG_DIR=") || strings.Contains(unpinnedDump, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
		t.Fatalf("unpinned repo leaked pinned seat config dir:\n%s", unpinnedDump)
	}

	// 4. Fail-closed pre-spawn checks: empty marker, unknown seat, collision with ~/.claude.
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, marker := range map[string]string{
		"empty-marker":   "   \n",
		"unknown-seat":   "nonexistent-seat\n",
		"invalid-syntax": "../escape\n",
	} {
		t.Run(name, func(t *testing.T) {
			badRepo := filepath.Join(home, "Code", name)
			if err := os.MkdirAll(badRepo, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(badRepo, ".claude-account"), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := StartClaude(t.Context(), Options{Program: bin, Cwd: badRepo, Mode: "ask"})
			if s != nil || err == nil || !NoProcessStarted(err) {
				if s != nil {
					_, _ = s.Close()
				}
				t.Fatalf("%s: expected proven pre-spawn refusal, got s=%v err=%v", name, s, err)
			}
		})
	}
}

func TestClaudeSeatSanitizesAmbientEnvForPersonalAndNamedSeats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	defaultClaude := filepath.Join(home, ".claude")
	seatChandra := filepath.Join(home, ".claude-chandra")
	customAmbientDir := filepath.Join(home, ".claude-arbitrary-shell-export")
	for _, dir := range []string{defaultClaude, seatChandra, customAmbientDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	accountsFile := filepath.Join(home, ".claude-accounts")
	if err := os.WriteFile(accountsFile, []byte("chandra ~/.claude-chandra\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate launching Manvi from inside a Claude Code shell that also exported
	// arbitrary config overrides and token overrides.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-ambient-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-ambient-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-ambient-leak")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CONFIG_DIR", customAmbientDir)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", customAmbientDir)

	assertNoLeakedKeys := func(t *testing.T, label, dump string, forbidConfigDirs bool) {
		t.Helper()
		lines := "\n" + dump + "\n"
		for _, key := range []string{
			"ANTHROPIC_API_KEY=",
			"ANTHROPIC_AUTH_TOKEN=",
			"CLAUDE_CODE_OAUTH_TOKEN=",
			"CLAUDECODE=",
			"CLAUDE_CODE_ENTRYPOINT=",
		} {
			if strings.Contains(lines, "\n"+key) {
				t.Fatalf("%s leaked %s:\n%s", label, key, dump)
			}
		}
		if forbidConfigDirs {
			for _, key := range []string{"CLAUDE_CONFIG_DIR=", "CLAUDE_SECURESTORAGE_CONFIG_DIR="} {
				if strings.Contains(lines, "\n"+key) {
					t.Fatalf("%s leaked %s:\n%s", label, key, dump)
				}
			}
		}
	}

	// 1. Unpinned repo on multi-seat host resolves to personal and strips ALL
	// overrides including arbitrary CLAUDE_CONFIG_DIR, CLAUDE_CODE_OAUTH_TOKEN, and CLAUDECODE.
	unpinnedRepo := filepath.Join(home, "Code", "Unpinned")
	if err := os.MkdirAll(unpinnedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	unpinnedDump := openClaudeSeatFixture(t, unpinnedRepo)
	assertNoLeakedKeys(t, "unpinned personal", unpinnedDump, true)

	// 2. Explicit .claude-account = personal strips all keys too.
	personalRepo := filepath.Join(home, "Code", "ExplicitPersonal")
	if err := os.MkdirAll(personalRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personalRepo, ".claude-account"), []byte("personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	personalDump := openClaudeSeatFixture(t, personalRepo)
	assertNoLeakedKeys(t, "explicit personal", personalDump, true)

	// 3. Named seat strips ambient keys cleanly (leaving zero duplicate CLAUDE_CONFIG_DIR)
	// before setting the pinned seat's directory.
	pinnedRepo := filepath.Join(home, "Code", "Pinned")
	if err := os.MkdirAll(pinnedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinnedRepo, ".claude-account"), []byte("chandra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedChandra, err := filepath.EvalSymlinks(seatChandra)
	if err != nil {
		t.Fatal(err)
	}
	pinnedDump := openClaudeSeatFixture(t, pinnedRepo)
	assertNoLeakedKeys(t, "named seat", pinnedDump, false)
	if strings.Count("\n"+pinnedDump, "\nCLAUDE_CONFIG_DIR=") != 1 ||
		!strings.Contains("\n"+pinnedDump, "\nCLAUDE_CONFIG_DIR="+resolvedChandra) {
		t.Fatalf("named seat CLAUDE_CONFIG_DIR not cleanly replaced:\n%s", pinnedDump)
	}
	if strings.Count("\n"+pinnedDump, "\nCLAUDE_SECURESTORAGE_CONFIG_DIR=") != 1 ||
		!strings.Contains("\n"+pinnedDump, "\nCLAUDE_SECURESTORAGE_CONFIG_DIR="+resolvedChandra) {
		t.Fatalf("named seat CLAUDE_SECURESTORAGE_CONFIG_DIR not cleanly replaced:\n%s", pinnedDump)
	}

	// 4. Even without ~/.claude-accounts, an unpinned repo strips inherited
	// CLAUDE_CONFIG_DIR, CLAUDE_SECURESTORAGE_CONFIG_DIR, CLAUDECODE, and CLAUDE_CODE_ENTRYPOINT.
	if err := os.Remove(accountsFile); err != nil {
		t.Fatal(err)
	}
	singleAccountDump := openClaudeSeatFixture(t, unpinnedRepo)
	for _, key := range []string{"CLAUDE_CONFIG_DIR=", "CLAUDE_SECURESTORAGE_CONFIG_DIR=", "CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT="} {
		if strings.Contains("\n"+singleAccountDump, "\n"+key) {
			t.Fatalf("single-account unpinned repo leaked %s:\n%s", key, singleAccountDump)
		}
	}
}

func TestClaudeSeatDefaultLoginCollisionGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	defaultClaude := filepath.Join(home, ".claude")
	seatChandra := filepath.Join(home, ".claude-chandra")
	seatLappi := filepath.Join(home, ".claude-lappi")
	unpinnedRepo := filepath.Join(home, "Code", "Unpinned")
	explicitPersonalRepo := filepath.Join(home, "Code", "Personal")
	pinnedRepo := filepath.Join(home, "Code", "Pinned")
	for _, dir := range []string{defaultClaude, seatChandra, seatLappi, unpinnedRepo, explicitPersonalRepo, pinnedRepo} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(explicitPersonalRepo, ".claude-account"), []byte("personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinnedRepo, ".claude-account"), []byte("chandra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	accountsFile := filepath.Join(home, ".claude-accounts")
	if err := os.WriteFile(accountsFile, []byte("chandra ~/.claude-chandra\nlappi ~/.claude-lappi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Zero named seats with emails -> personal succeeds without ~/.claude.json.
	_ = openClaudeSeatFixture(t, unpinnedRepo)

	// Populate seat email on chandra.
	if err := os.WriteFile(
		filepath.Join(seatChandra, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"chandra@scholarlm.dev"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	// Case A: ~/.claude.json signed into same email (case-insensitive) -> fail closed on personal/unpinned!
	if err := os.WriteFile(
		filepath.Join(home, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"CHANDRA@ScholarLM.dev"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for label, repo := range map[string]string{"unpinned": unpinnedRepo, "explicit-personal": explicitPersonalRepo} {
		s, startErr := StartClaude(t.Context(), Options{Program: bin, Cwd: repo, Mode: "ask"})
		if s != nil {
			_, _ = s.Close()
		}
		if startErr == nil || !NoProcessStarted(startErr) {
			t.Fatalf("%s: expected pre-spawn collision refusal, got s=%v err=%v", label, s, startErr)
		}
		msg := startErr.Error()
		if !strings.Contains(msg, "chandra") || !strings.Contains(msg, ".claude-account") {
			t.Fatalf("%s: error message lacks actionable seat guidance: %q", label, msg)
		}
	}
	// Pinned repo still launches cleanly even while default ~/.claude.json collides!
	_ = openClaudeSeatFixture(t, pinnedRepo)

	// Case B: Malformed ~/.claude.json when named seats with emails exist -> fail closed!
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{corrupt-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, startErr := StartClaude(t.Context(), Options{Program: bin, Cwd: unpinnedRepo, Mode: "ask"}); s != nil || startErr == nil || !NoProcessStarted(startErr) {
		if s != nil {
			_, _ = s.Close()
		}
		t.Fatalf("malformed default ~/.claude.json was accepted when named seats exist: s=%v err=%v", s, startErr)
	}

	// Case C: Distinct email in ~/.claude.json -> personal succeeds!
	if err := os.WriteFile(
		filepath.Join(home, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"personal@example.com"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	_ = openClaudeSeatFixture(t, unpinnedRepo)
	_ = openClaudeSeatFixture(t, explicitPersonalRepo)

	// Case D: Malformed <seatDir>/.claude.json -> fail closed on personal because seat identity cannot be checked!
	if err := os.WriteFile(filepath.Join(seatLappi, ".claude.json"), []byte(`{bad-seat-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, startErr := StartClaude(t.Context(), Options{Program: bin, Cwd: unpinnedRepo, Mode: "ask"}); s != nil || startErr == nil || !NoProcessStarted(startErr) {
		if s != nil {
			_, _ = s.Close()
		}
		t.Fatalf("malformed seat .claude.json did not fail closed: s=%v err=%v", s, startErr)
	}
}

func TestClaudeSeatLinkedWorktreeSubdirectoryInheritsMainCheckoutSeat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	seatChandra := filepath.Join(home, ".claude-chandra")
	if err := os.MkdirAll(seatChandra, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".claude-accounts"),
		[]byte("chandra "+seatChandra+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	resolvedChandra, err := filepath.EvalSymlinks(seatChandra)
	if err != nil {
		t.Fatal(err)
	}

	mainRepo := filepath.Join(home, "Code", "MainRepo")
	worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", "wt-sub")
	if err := os.MkdirAll(worktreeGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainRepo, ".claude-account"), []byte("chandra\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	linkedWorktree := filepath.Join(home, "Worktrees", "wt-sub")
	subDir := filepath.Join(linkedWorktree, "services", "api")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(linkedWorktree, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	dump := openClaudeSeatFixture(t, subDir)
	lines := "\n" + dump + "\n"
	if !strings.Contains(lines, "\nCLAUDE_CONFIG_DIR="+resolvedChandra+"\n") {
		t.Fatalf("subdirectory inside linked worktree did not inherit main checkout .claude-account seat:\n%s", dump)
	}
	if !strings.Contains(lines, "\nCLAUDE_SECURESTORAGE_CONFIG_DIR="+resolvedChandra+"\n") {
		t.Fatalf("subdirectory inside linked worktree missing CLAUDE_SECURESTORAGE_CONFIG_DIR:\n%s", dump)
	}
}



