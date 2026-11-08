package codingagent

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// Adversarial regression tests for lane w16-agy, criterion B (seat routing).

type advSeatWorld struct {
	home, repo, seatWork, seatLappi string
}

// advSeats builds a HOME with `work` and `lappi` seats mapped and a plain repo.
func advSeats(t *testing.T) advSeatWorld {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path fixtures")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	w := advSeatWorld{
		home:      home,
		repo:      filepath.Join(home, "Code", "proj"),
		seatWork:  filepath.Join(home, ".claude-work"),
		seatLappi: filepath.Join(home, ".claude-lappi"),
	}
	for _, d := range []string{w.repo, w.seatWork, w.seatLappi, filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	advWrite(t, filepath.Join(home, ".claude-accounts"), "work ~/.claude-work\nlappi ~/.claude-lappi\n")
	return w
}

func advWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func advSeatDirOf(t *testing.T, o envOverrides) string {
	t.Helper()
	for _, kv := range o.set {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return v
		}
	}
	return ""
}

// --- ruling 2 -------------------------------------------------------------

// With no seats, the child receives the operator's environment unchanged except
// the nested-session markers. ANTHROPIC_API_KEY is withheld by childenv before
// this layer (see the report); everything else the operator set must survive.
func TestAdvW16NoSeatsEnvReachesTheChildExceptNestedMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := filepath.Join(home, "Code", "Plain")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, ".claude-custom")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", custom)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "operator-oauth")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "operator-auth-token")
	t.Setenv("ANTHROPIC_API_KEY", "operator-api-key")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")

	o, err := claudeSeatOverrides(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := applyEnvOverrides(childenv.Inherit(), o)
	has := func(key string) bool {
		return slices.ContainsFunc(got, func(e string) bool { return strings.HasPrefix(e, key+"=") })
	}
	for _, key := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		if has(key) {
			t.Errorf("%s reached the child", key)
		}
	}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK"} {
		if !has(key) {
			t.Errorf("no seats configured, but operator's %s did not reach the child", key)
		}
	}
	if !has("ANTHROPIC_AUTH_TOKEN") {
		t.Logf("observation: ANTHROPIC_AUTH_TOKEN does not reach the child with no seats either (childenv.Inherit withholds it)")
	}
	if !has("ANTHROPIC_API_KEY") {
		t.Logf("observation: ANTHROPIC_API_KEY does not reach the child even with no seats (childenv.Inherit withholds it before seat routing runs), so ruling 2's API-key half is moot")
	}
}

// With seats active the credential and config variables are gone and the seat's
// are set, for named seats and for `personal`.
func TestAdvW16SeatsActiveStripsAndSets(t *testing.T) {
	w := advSeats(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/ambient")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/ambient")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "ambient")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient")
	advWrite(t, filepath.Join(w.repo, ".claude-account"), "work\n")
	o, err := claudeSeatOverrides(w.repo)
	if err != nil {
		t.Fatal(err)
	}
	env := applyEnvOverrides(append(childenv.Inherit(), "ANTHROPIC_API_KEY=x", "CLAUDE_CONFIG_DIR=/ambient"), o)
	for _, e := range env {
		for _, bad := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CONFIG_DIR=/ambient", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/ambient"} {
			if strings.HasPrefix(e, bad) {
				t.Errorf("seat routing left %q in the child env", e)
			}
		}
	}
	if d := advSeatDirOf(t, o); d != resolvedOrSelf(w.seatWork) {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", d, resolvedOrSelf(w.seatWork))
	}
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(e, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("child env has %d seat config entries, want exactly 2 (no duplicates)", count)
	}
}

func resolvedOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// Windows-style case-folded env names are removed too.
func TestAdvW16SeatStripFoldsCaseWhereTheOSDoes(t *testing.T) {
	o := envOverrides{unsetKeys: []string{"ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR"}, set: []string{"CLAUDE_CONFIG_DIR=/seat"}}
	got := applyEnvOverridesKeyed([]string{"anthropic_api_key=k", "Claude_Config_Dir=/amb", "KEEP=1"}, o, childenv.KeyFor(true))
	if !slices.Equal(got, []string{"KEEP=1", "CLAUDE_CONFIG_DIR=/seat"}) {
		t.Fatalf("got %v", got)
	}
}

// --- ruling 3: marker content --------------------------------------------

func TestAdvW16MarkerContentVariants(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // "seat" selects work, "err" must fail closed
	}{
		{"CRLF", "work\r\n", "seat"},
		{"surrounding whitespace", "  work \t\n\n", "seat"},
		{"BOM", "\xef\xbb\xbfwork\n", "err"},
		{"NUL inside", "wo\x00rk\n", "err"},
		{"NUL only", "\x00", "err"},
		{"traversal", "../../etc\n", "err"},
		{"slash", "work/../lappi\n", "err"},
		{"absolute", "/tmp/x\n", "err"},
		{"dot", ".\n", "err"},
		{"dotdot", "..\n", "err"},
		{"tilde", "~/.claude-work\n", "err"},
		{"unknown", "stranger\n", "err"},
		{"case variant of a seat", "WORK\n", "err"},
		{"oversize", strings.Repeat("a", 5000), "err"},
		{"empty", "", "err"},
		{"whitespace only", " \n", "err"},
		{"unicode lookalike", "w\xd0\xberk\n", "err"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := advSeats(t)
			advWrite(t, filepath.Join(w.repo, ".claude-account"), tc.content)
			o, err := claudeSeatOverrides(w.repo)
			switch tc.want {
			case "seat":
				if err != nil || advSeatDirOf(t, o) != resolvedOrSelf(w.seatWork) {
					t.Fatalf("got %+v, %v; want the work seat", o, err)
				}
			case "err":
				if err == nil {
					t.Fatalf("marker %q was accepted: %+v", tc.content, o)
				}
			}
		})
	}
}

// A marker that selects a seat must select one the operator mapped; a seat named
// in the marker but absent from the accounts file, or a marker with no accounts
// file, fails closed.
func TestAdvW16MarkerCannotSelectAnUnmappedSeatOrDirectory(t *testing.T) {
	w := advSeats(t)
	// A real directory that exists but is not mapped.
	if err := os.MkdirAll(filepath.Join(w.home, ".claude-evil"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"evil", ".claude-evil", "../.claude-evil", w.home + "/.claude-evil"} {
		advWrite(t, filepath.Join(w.repo, ".claude-account"), m)
		if o, err := claudeSeatOverrides(w.repo); err == nil {
			t.Errorf("marker %q selected %+v", m, o)
		}
	}
	if err := os.Remove(filepath.Join(w.home, ".claude-accounts")); err != nil {
		t.Fatal(err)
	}
	advWrite(t, filepath.Join(w.repo, ".claude-account"), "work")
	if o, err := claudeSeatOverrides(w.repo); err == nil {
		t.Errorf("pinned seat without ~/.claude-accounts was accepted: %+v", o)
	}
}

// A marker that is a symlink to some other file must not be followed: its
// content would be read, and the first token echoed in the error.
func TestAdvW16SymlinkedMarkerDoesNotLeakTheTargetsContent(t *testing.T) {
	w := advSeats(t)
	secret := "sk-ant-api03-SUPERSECRETTOKENVALUE"
	target := filepath.Join(w.home, ".some-token-file")
	advWrite(t, target, secret+"\n")
	if err := os.Symlink(target, filepath.Join(w.repo, ".claude-account")); err != nil {
		t.Fatal(err)
	}
	o, err := claudeSeatOverrides(w.repo)
	if err != nil && strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("error leaks the content of the file a marker symlink points at: %v", err)
	}
	if err == nil && advSeatDirOf(t, o) != "" {
		t.Fatalf("a symlinked marker selected a seat: %+v", o)
	}
}

// The same leak through the "invalid seat name" path: a target whose first token
// is not a legal name (JSON, key=value) is echoed verbatim in the error.
func TestAdvW16SymlinkedMarkerDoesNotLeakNonNameContent(t *testing.T) {
	for name, content := range map[string]string{
		"json":      `{"primaryApiKey":"sk-ant-JSONSECRET"}`,
		"key=value": "ANTHROPIC_API_KEY=sk-ant-ENVSECRET\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := advSeats(t)
			target := filepath.Join(w.home, ".claude.json")
			advWrite(t, target, content)
			if err := os.Symlink(target, filepath.Join(w.repo, ".claude-account")); err != nil {
				t.Fatal(err)
			}
			_, err := claudeSeatOverrides(w.repo)
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error leaks the first token of the file a marker symlink points at: %v", err)
			}
		})
	}
}

// A symlinked marker whose target holds a real seat name: the checkout can point
// the marker at a file outside itself (here the operator's own accounts file,
// whose first token is a seat name). It should not be honoured.
func TestAdvW16SymlinkedMarkerIsNotHonoured(t *testing.T) {
	w := advSeats(t)
	target := filepath.Join(w.home, "elsewhere", "seatname")
	advWrite(t, target, "lappi\n")
	if err := os.Symlink(target, filepath.Join(w.repo, ".claude-account")); err != nil {
		t.Fatal(err)
	}
	o, err := claudeSeatOverrides(w.repo)
	if err == nil && advSeatDirOf(t, o) != "" {
		t.Fatalf("symlinked .claude-account selected seat dir %q", advSeatDirOf(t, o))
	}
}

// The ancestor walk stops at HOME. A case-variant spelling of the checkout path
// must not walk through HOME and read a marker that lives in HOME itself.
func TestAdvW16CaseVariantCwdDoesNotReadAMarkerInHome(t *testing.T) {
	w := advSeats(t)
	advWrite(t, filepath.Join(w.home, ".claude-account"), "work\n")
	variant := strings.ToUpper(w.repo)
	if _, err := os.Stat(variant); err != nil {
		t.Skip("case-sensitive volume")
	}
	o, err := claudeSeatOverrides(variant)
	if err == nil && advSeatDirOf(t, o) != "" {
		t.Fatalf("a marker in $HOME itself selected %q via a case-variant cwd", advSeatDirOf(t, o))
	}
}

// A checkout outside $HOME has no "below HOME" bound: the walk reads markers in
// every ancestor up to "/". A marker in a shared parent of a checkout outside
// $HOME must not pin it.
func TestAdvW16MarkerAboveACheckoutOutsideHomeIsNotHonoured(t *testing.T) {
	w := advSeats(t)
	shared := t.TempDir() // not under w.home
	advWrite(t, filepath.Join(shared, ".claude-account"), "work\n")
	cwd := filepath.Join(shared, "checkout")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	o, err := claudeSeatOverrides(cwd)
	if err == nil && advSeatDirOf(t, o) != "" {
		t.Fatalf("a marker above the checkout root (%s) pinned seat %q", shared, advSeatDirOf(t, o))
	}
	_ = w
}

// A linked worktree whose .git file and commondir are written by the checkout
// itself, pointing at another repository, picks up that repository's marker.
// Git itself requires the back-pointer <gitdir>/gitdir to name this worktree.
func TestAdvW16ForgedLinkedWorktreeDoesNotBorrowAnotherCheckoutsMarker(t *testing.T) {
	w := advSeats(t)
	victim := filepath.Join(w.home, "Code", "victim")
	if err := os.MkdirAll(filepath.Join(victim, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	advWrite(t, filepath.Join(victim, ".claude-account"), "lappi\n")

	evil := filepath.Join(w.home, "Code", "evil")
	fakeGitDir := filepath.Join(evil, "fake-gitdir")
	advWrite(t, filepath.Join(fakeGitDir, "commondir"), filepath.Join(victim, ".git"))
	advWrite(t, filepath.Join(evil, ".git"), "gitdir: "+fakeGitDir+"\n")

	o, err := claudeSeatOverrides(evil)
	if err == nil && advSeatDirOf(t, o) != "" {
		t.Fatalf("forged worktree borrowed the marker of %s: seat %q", victim, advSeatDirOf(t, o))
	}
}

// --- ruling 3: accounts map ----------------------------------------------

func TestAdvW16AccountsFileAttacks(t *testing.T) {
	cases := []struct {
		name  string
		lines func(w advSeatWorld) string
	}{
		{"duplicate seat name, different dirs", func(w advSeatWorld) string {
			return "work ~/.claude-work\nwork ~/.claude-lappi\n"
		}},
		{"traversal in seat name", func(w advSeatWorld) string { return "../x ~/.claude-work\n" }},
		{"relative dir", func(w advSeatWorld) string { return "work .claude-work\n" }},
		{"dir missing", func(w advSeatWorld) string { return "work ~/does-not-exist\n" }},
		{"default dir plain", func(w advSeatWorld) string { return "work ~/.claude\n" }},
		{"default dir trailing slash", func(w advSeatWorld) string { return "work ~/.claude/\n" }},
		{"default dir dot", func(w advSeatWorld) string { return "work ~/.claude/.\n" }},
		{"default dir via dot dot", func(w advSeatWorld) string { return "work ~/.claude-work/../.claude\n" }},
		{"default dir absolute", func(w advSeatWorld) string { return "work " + w.home + "/.claude\n" }},
		{"two seats one dir", func(w advSeatWorld) string { return "work ~/.claude-work\nlappi ~/.claude-work/\n" }},
		{"BOM first line", func(w advSeatWorld) string { return "\xef\xbb\xbfwork ~/.claude-work\n" }},
		{"trailing comment glued to dir", func(w advSeatWorld) string { return "work ~/.claude-work # mine\n" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := advSeats(t)
			advWrite(t, filepath.Join(w.home, ".claude-accounts"), tc.lines(w))
			advWrite(t, filepath.Join(w.repo, ".claude-account"), "work")
			o, err := claudeSeatOverrides(w.repo)
			if err == nil {
				t.Fatalf("accounts file accepted; selected %q", advSeatDirOf(t, o))
			}
		})
	}
}

func TestAdvW16SeatDirEqualToDefaultUnderAnotherSpelling(t *testing.T) {
	t.Run("symlink to ~/.claude", func(t *testing.T) {
		w := advSeats(t)
		if err := os.Symlink(filepath.Join(w.home, ".claude"), filepath.Join(w.home, "alias")); err != nil {
			t.Fatal(err)
		}
		advWrite(t, filepath.Join(w.home, ".claude-accounts"), "work ~/alias\n")
		advWrite(t, filepath.Join(w.repo, ".claude-account"), "work")
		if o, err := claudeSeatOverrides(w.repo); err == nil {
			t.Fatalf("symlink to ~/.claude accepted as a seat: %q", advSeatDirOf(t, o))
		}
	})
	t.Run("case variant", func(t *testing.T) {
		w := advSeats(t)
		if _, err := os.Stat(filepath.Join(w.home, ".CLAUDE")); err != nil {
			t.Skip("case-sensitive volume")
		}
		advWrite(t, filepath.Join(w.home, ".claude-accounts"), "work ~/.CLAUDE\n")
		advWrite(t, filepath.Join(w.repo, ".claude-account"), "work")
		if o, err := claudeSeatOverrides(w.repo); err == nil {
			t.Fatalf("~/.CLAUDE (same directory as ~/.claude on this volume) accepted as a seat: %q", advSeatDirOf(t, o))
		}
	})
	t.Run("case variant shares a dir with another seat", func(t *testing.T) {
		w := advSeats(t)
		if _, err := os.Stat(filepath.Join(w.home, ".CLAUDE-WORK")); err != nil {
			t.Skip("case-sensitive volume")
		}
		advWrite(t, filepath.Join(w.home, ".claude-accounts"), "work ~/.claude-work\nlappi ~/.CLAUDE-WORK\n")
		advWrite(t, filepath.Join(w.repo, ".claude-account"), "work")
		if o, err := claudeSeatOverrides(w.repo); err == nil {
			t.Fatalf("two seats name one directory under different case: %q", advSeatDirOf(t, o))
		}
	})
}

// A pinned marker with an unusable HOME must not silently run the launch on the
// ambient (default) account.
func TestAdvW16PinnedMarkerWithUnusableHomeFailsClosed(t *testing.T) {
	for name, home := range map[string]string{"unset": "", "relative": "relhome"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX")
			}
			repo := t.TempDir()
			advWrite(t, filepath.Join(repo, ".claude-account"), "work\n")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			o, err := claudeSeatOverrides(repo)
			if err == nil {
				t.Fatalf("a repository pinned to seat \"work\" launched unrouted with HOME=%q: %+v", home, o)
			}
		})
	}
}

// --- bounded, non-hanging, no content in errors -------------------------

func TestAdvW16HugeSeatFilesAreBoundedAndFast(t *testing.T) {
	for name, path := range map[string]func(w advSeatWorld) string{
		"marker":   func(w advSeatWorld) string { return filepath.Join(w.repo, ".claude-account") },
		"accounts": func(w advSeatWorld) string { return filepath.Join(w.home, ".claude-accounts") },
		"seat json": func(w advSeatWorld) string {
			return filepath.Join(w.seatWork, ".claude.json")
		},
		"home json": func(w advSeatWorld) string { return filepath.Join(w.home, ".claude.json") },
	} {
		t.Run(name, func(t *testing.T) {
			w := advSeats(t)
			advWrite(t, filepath.Join(w.seatWork, ".claude.json"), `{"oauthAccount":{"emailAddress":"w@example.com"}}`)
			p := path(w)
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(2 << 30); err != nil { // 2 GiB sparse
				t.Skipf("sparse files unavailable: %v", err)
			}
			_ = f.Close()
			if _, err := claudeSeatOverrides(w.repo); err == nil {
				t.Fatalf("a 2 GiB %s was accepted", name)
			}
		})
	}
}

// No token or .claude.json content appears in any error string.
func TestAdvW16ErrorsNeverCarryConfigContent(t *testing.T) {
	secret := "sk-ant-oat01-TOPSECRETCONTENT"
	payloads := []string{
		secret,
		`{"oauthAccount":{"emailAddress":"a@b"},"token":"` + secret,
		`{"oauthAccount":"` + secret + `"}`,
		`{"oauthAccount":{"emailAddress":["` + secret + `"]}}`,
		"{\"k\":" + secret + "}",
		"\x00" + secret,
	}
	for i, payload := range payloads {
		for _, where := range []string{"seat", "home"} {
			w := advSeats(t)
			advWrite(t, filepath.Join(w.seatLappi, ".claude.json"), `{"oauthAccount":{"emailAddress":"l@example.com"}}`)
			target := filepath.Join(w.seatWork, ".claude.json")
			if where == "home" {
				target = filepath.Join(w.home, ".claude.json")
			}
			advWrite(t, target, payload)
			_, err := claudeSeatOverrides(w.repo)
			if err != nil && strings.Contains(err.Error(), "TOPSECRET") {
				t.Errorf("payload %d in %s .claude.json leaked into the error: %v", i, where, err)
			}
		}
	}
}

// The seat check reports an email address, which is identity, not a token; it
// must not repeat any other field of the file.
func TestAdvW16MismatchErrorDoesNotEchoOtherFields(t *testing.T) {
	w := advSeats(t)
	cfg := `{"oauthAccount":{"emailAddress":"same@example.com"},"primaryApiKey":"sk-ant-LEAKME"}`
	advWrite(t, filepath.Join(w.seatWork, ".claude.json"), cfg)
	advWrite(t, filepath.Join(w.home, ".claude.json"), cfg)
	_, err := claudeSeatOverrides(w.repo)
	if err == nil {
		t.Fatal("default login equal to a seat login was accepted")
	}
	if strings.Contains(err.Error(), "LEAKME") {
		t.Fatalf("error echoes config content: %v", err)
	}
}
