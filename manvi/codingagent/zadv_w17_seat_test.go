package codingagent

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Adversarial cover for w17 lane C (ft-30c6322be987b6ab4c178cbf1e77197c).

const (
	zadvW17SharedMarker = "shared-marker-zz"
	// Not a valid seat identifier (':' and '@'), so it may never appear in an error.
	zadvW17Canary      = "canary:9c1d"
	zadvW17EmailCanary = "w17-canary@leak.invalid"
)

func zadvW17Chain(err error) []error {
	var out []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// zadvW17AssertClean checks every node of the wrap chain, the %v/%+v/%q
// renderings and any *fs.PathError.Path for the canaries and for raw control
// bytes or invalid UTF-8.
func zadvW17AssertClean(t *testing.T, err error, canaries ...string) {
	t.Helper()
	renderings := []string{fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%q", err)}
	for _, node := range zadvW17Chain(err) {
		renderings = append(renderings, node.Error())
		var pe *fs.PathError
		if errors.As(node, &pe) {
			renderings = append(renderings, pe.Path, pe.Op)
		}
	}
	for _, msg := range renderings {
		for _, c := range canaries {
			if strings.Contains(msg, c) {
				t.Fatalf("error text carries operator-file bytes %q: %q", c, msg)
			}
		}
	}
	// %q legitimately escapes; control/UTF-8 checks apply to the raw texts.
	for _, node := range zadvW17Chain(err) {
		msg := node.Error()
		if !utf8.ValidString(msg) {
			t.Fatalf("error text is not valid UTF-8: %q", msg)
		}
		for i := 0; i < len(msg); i++ {
			if msg[i] < 0x20 || msg[i] == 0x7f {
				t.Fatalf("error text carries control byte %#x: %q", msg[i], msg)
			}
		}
	}
}

func zadvW17Write(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func zadvW17SetHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// sharedWorld: a shared ancestor holding a marker, with a checkout below it.
func zadvW17SharedWorld(t *testing.T) (shared, checkout string) {
	t.Helper()
	shared = t.TempDir()
	zadvW17Write(t, filepath.Join(shared, ".claude-account"), []byte(zadvW17SharedMarker+"\n"))
	checkout = filepath.Join(shared, "proj", "sub")
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatal(err)
	}
	return shared, checkout
}

func zadvW17Spellings(t *testing.T) map[string]string {
	t.Helper()
	sep := string(filepath.Separator)
	tmp := t.TempDir()
	out := map[string]string{
		"root":             sep,
		"double":           sep + sep,
		"triple":           sep + sep + sep,
		"dot-slash":        sep + "." + sep,
		"dotdot-slash":     sep + ".." + sep,
		"dotdot-dotdot":    sep + ".." + sep + ".." + sep + ".." + sep,
		"tmp-dotdot":       sep + "tmp" + sep + "..",
		"tmp-dotdot-slash": sep + "tmp" + sep + ".." + sep,
		"private-dotdot":   sep + "private" + sep + "..",
	}
	if runtime.GOOS == "windows" {
		return map[string]string{"drive": `C:\`, "unc": `\\server\share\`}
	}
	// symlink chain to the root.
	l3 := filepath.Join(tmp, "l3")
	l2 := filepath.Join(tmp, "l2")
	l1 := filepath.Join(tmp, "l1")
	if os.Symlink(sep, l3) == nil && os.Symlink(l3, l2) == nil && os.Symlink(l2, l1) == nil {
		out["symlink-chain-to-root"] = l1
		out["symlink-chain-slash"] = l1 + sep
		out["symlink-dotdot"] = filepath.Join(tmp, "l3") + sep + ".."
	}
	// a deep real dir reached with enough ".." to climb to the root.
	deep := filepath.Join(tmp, "a", "b")
	if os.MkdirAll(deep, 0o750) == nil {
		up := strings.Repeat(".."+sep, 40)
		out["deep-dotdot-to-root"] = deep + sep + up
	}
	return out
}

// Criterion 1: every spelling of the root is refused when the checkout pins a
// seat (through the entry point), and never lets a shared-ancestor marker pin.
func TestZadvW17RootSpellingsAreRefusedAndNeverWalkUp(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, home := range zadvW17Spellings(t) {
		t.Run(name, func(t *testing.T) {
			zadvW17SetHome(t, home)
			if usableClaudeHome(home) {
				t.Fatalf("usableClaudeHome(%q) = true", home)
			}
			// pinned in the checkout itself: refusal, before any spawn.
			pinned := t.TempDir()
			zadvW17Write(t, filepath.Join(pinned, ".claude-account"), []byte("chandra\n"))
			_, ovErr := claudeSeatOverrides(pinned)
			if ovErr == nil || !strings.Contains(ovErr.Error(), "HOME is not a usable") {
				t.Fatalf("HOME=%q pinned checkout: want unusable-HOME refusal, got %v", home, ovErr)
			}
			s, startErr := StartClaude(t.Context(), Options{Program: bin, Cwd: pinned, Mode: "ask"})
			if s != nil {
				_, _ = s.Close()
				t.Fatalf("HOME=%q: process started", home)
			}
			if startErr == nil || !NoProcessStarted(startErr) {
				t.Fatalf("HOME=%q: want NoProcessStarted refusal, got %v", home, startErr)
			}
			zadvW17AssertClean(t, startErr, "chandra")
			// shared ancestor marker: must not be seen at all.
			_, checkout := zadvW17SharedWorld(t)
			ov, sErr := claudeSeatOverrides(checkout)
			if sErr != nil || len(ov.set) != 0 {
				t.Fatalf("HOME=%q: shared-ancestor marker steered the launch: ov=%+v err=%v", home, ov, sErr)
			}
		})
	}
}

// HOME values that are unusable-by-accident or odd must still never let the
// walk climb above the checkout.
func TestZadvW17OddHomesNeverWalkAboveTheCheckout(t *testing.T) {
	tmp := t.TempDir()
	loopA := filepath.Join(tmp, "loopA")
	loopB := filepath.Join(tmp, "loopB")
	loopOK := os.Symlink(loopB, loopA) == nil && os.Symlink(loopA, loopB) == nil
	file := filepath.Join(tmp, "afile")
	zadvW17Write(t, file, []byte("x"))
	homes := map[string]string{
		"nonexistent":      filepath.Join(tmp, "does", "not", "exist"),
		"regular file":     file,
		"too long":         "/" + strings.Repeat("a", 5000),
		"trailing space":   "/ ",
		"trailing newline": "/\n",
	}
	if loopOK {
		homes["eloop"] = loopA
		homes["eloop-child"] = filepath.Join(loopA, "x")
	}
	for name, home := range homes {
		t.Run(name, func(t *testing.T) {
			_, checkout := zadvW17SharedWorld(t)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			ov, err := claudeSeatOverrides(checkout)
			if len(ov.set) != 0 {
				t.Fatalf("shared-ancestor marker pinned a seat with HOME=%q: %v", home, ov.set)
			}
			if err != nil {
				for _, bad := range []string{zadvW17SharedMarker, "pinned .claude-account", "unknown .claude-account", "empty .claude-account", "invalid .claude-account"} {
					if strings.Contains(err.Error(), bad) {
						t.Fatalf("HOME=%q: the shared-ancestor marker was read (%q): %v", home, bad, err)
					}
				}
			}
		})
	}
}

// A NUL cannot be exported through the environment, so the predicate and the
// walk are driven directly.
func TestZadvW17NulHomeNeverWalksUp(t *testing.T) {
	_, checkout := zadvW17SharedWorld(t)
	for _, home := range []string{"/\x00", "/\x00/..", "\x00"} {
		if got := markerSearchDirs(checkout, home); len(got) > 2 {
			t.Fatalf("markerSearchDirs with HOME=%q walked %d dirs: %v", home, len(got), got)
		}
		if usableClaudeHome(home) {
			// usable-by-accident is only tolerable because the walk stays bounded (checked above).
			t.Logf("usableClaudeHome(%q) = true (walk bounded)", home)
		}
	}
}

// Criterion 1, judgement part: HOME naming a shared parent of many users' trees.
// The card says "any HOME that is an ancestor of many users' trees"; the fix
// refused only the root. BREAK candidate: each of these is accepted.
func TestZadvW17SharedParentHomesAreNotUsable(t *testing.T) {
	for _, p := range []string{"/Users", "/home", "/tmp", "/var/tmp", "/Users/Shared"} {
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			continue
		}
		t.Run(p, func(t *testing.T) {
			if usableClaudeHome(p) {
				t.Fatalf("usableClaudeHome(%q) = true: a marker at %s/<user>/.claude-account (or %s/<dir>/ for a shared dir) pins a daemon started with HOME=%s", p, p, p, p)
			}
		})
	}
}

// Demonstrates the attack the card names with a HOME that is a shared parent:
// a marker planted in a sibling user's tree steers the launch. Records current
// behaviour; the assertion is that the launch is NOT steered.
func TestZadvW17MarkerInSiblingUserTreeDoesNotSteerSharedParentHome(t *testing.T) {
	users := t.TempDir()
	checkout := filepath.Join(users, "alice", "proj")
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatal(err)
	}
	zadvW17Write(t, filepath.Join(users, "alice", ".claude-account"), []byte(zadvW17SharedMarker+"\n"))
	zadvW17SetHome(t, users)
	if !usableClaudeHome(users) {
		return // refused outright: attack closed
	}
	_, err := claudeSeatOverrides(checkout)
	if err != nil && strings.Contains(err.Error(), ".claude-account") {
		t.Fatalf("a marker in alice's tree was honoured under HOME=<parent of users>: %v", err)
	}
}

// Defense in depth: the walk itself must not climb to the root when handed a
// root HOME. Today safety depends on every caller running usableClaudeHome
// first (markerSearchDirs has no guard of its own).
func TestZadvW17MarkerSearchDirsHasItsOwnRootGuard(t *testing.T) {
	_, checkout := zadvW17SharedWorld(t)
	homes := []string{string(filepath.Separator)}
	link := filepath.Join(t.TempDir(), "rootlink")
	if os.Symlink(string(filepath.Separator), link) == nil {
		homes = append(homes, link)
	}
	for _, home := range homes {
		dirs := markerSearchDirs(checkout, home)
		for _, d := range dirs {
			if d != checkout && !zadvW17Exists(filepath.Join(d, ".git")) {
				t.Fatalf("markerSearchDirs(%q, %q) returned ancestor %q above the checkout: %v", checkout, home, d, dirs)
			}
		}
	}
}

func zadvW17Exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TOCTOU: the symlink is resolved once in usableClaudeHome and re-resolved by
// markerSearchDirs' own os.Stat(home). A swap between them lets a usable-looking
// HOME become "/" for the walk. Probabilistic: it can only fail when the swap
// lands in the window, never falsely.
func TestZadvW17HomeSymlinkSwapBetweenCheckAndWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink swap")
	}
	tmp := t.TempDir()
	realHome := filepath.Join(tmp, "realhome")
	if err := os.MkdirAll(realHome, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "home")
	mk := func(target, name string) string {
		p := filepath.Join(tmp, name)
		if err := os.Symlink(target, p); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return p
	}
	toReal := mk(realHome, "sw-real")
	if err := os.Rename(toReal, link); err != nil {
		t.Fatal(err)
	}
	_, checkout := zadvW17SharedWorld(t)
	zadvW17SetHome(t, link)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tgt := string(filepath.Separator)
			if i%2 == 1 {
				tgt = realHome
			}
			tmpLink := filepath.Join(tmp, fmt.Sprintf("swap-%d", i%2))
			_ = os.Remove(tmpLink)
			if os.Symlink(tgt, tmpLink) == nil {
				_ = os.Rename(tmpLink, link)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	for i := 0; i < 20000; i++ {
		ov, err := claudeSeatOverrides(checkout)
		if len(ov.set) != 0 {
			t.Fatalf("iteration %d: marker pinned a seat through a swapped HOME: %v", i, ov.set)
		}
		if err == nil {
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "HOME is not a usable") {
			continue
		}
		if strings.Contains(msg, ".claude-account") || strings.Contains(msg, zadvW17SharedMarker) {
			t.Fatalf("iteration %d: TOCTOU, the shared-ancestor marker was read after HOME was swapped to /: %v", i, err)
		}
	}
}

// --- criterion 2 ------------------------------------------------------------

type zadvW17World struct {
	home, seat, checkout string
}

func zadvW17NewWorld(t *testing.T, seatDirName string) zadvW17World {
	t.Helper()
	home := t.TempDir()
	zadvW17SetHome(t, home)
	seat := filepath.Join(home, seatDirName)
	if err := os.MkdirAll(seat, 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(home, "proj")
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatal(err)
	}
	return zadvW17World{home: home, seat: seat, checkout: checkout}
}

func zadvW17Email(e string) []byte {
	return []byte(`{"oauthAccount":{"emailAddress":` + zadvW17JSONQuote(e) + `}}`)
}

func zadvW17JSONQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestZadvW17ErrorsNeverCarryOperatorBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix permission semantics")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bomCanary := "\ufeff" + zadvW17Canary
	huge := zadvW17EmailCanary + strings.Repeat("A", 1<<20)
	type tc struct {
		name     string
		seatDir  string // seat directory name (may carry the canary)
		accounts string // %SEAT% = resolved seat dir
		seatJSON []byte
		homeJSON []byte
		marker   []byte
		setup    func(t *testing.T, w zadvW17World)
	}
	both := zadvW17Email(zadvW17EmailCanary)
	cases := []tc{
		{name: "email collision plain", accounts: "alpha %SEAT%\n", seatJSON: both, homeJSON: both},
		{name: "email collision with terminal escapes", accounts: "alpha %SEAT%\n",
			seatJSON: zadvW17Email("\x1b[31m\x07" + zadvW17EmailCanary), homeJSON: zadvW17Email("\x1b[31m\x07" + zadvW17EmailCanary)},
		{name: "email collision unicode and case", accounts: "alpha %SEAT%\n",
			seatJSON: zadvW17Email("Ünï-" + zadvW17EmailCanary), homeJSON: zadvW17Email("ünï-" + zadvW17EmailCanary)},
		{name: "email collision huge", accounts: "alpha %SEAT%\n", seatJSON: zadvW17Email(huge), homeJSON: zadvW17Email(huge)},
		{name: "email collision invalid utf8", accounts: "alpha %SEAT%\n",
			seatJSON: []byte("{\"oauthAccount\":{\"emailAddress\":\"\xff\xfe" + zadvW17EmailCanary + "\"}}"),
			homeJSON: []byte("{\"oauthAccount\":{\"emailAddress\":\"\xff\xfe" + zadvW17EmailCanary + "\"}}")},
		{name: "duplicate key canary", accounts: "alpha %SEAT%\n", seatJSON: both,
			homeJSON: []byte(`{"oauthAccount":{"emailAddress":"x@y"},"oauthAccount":{"emailAddress":"` + zadvW17EmailCanary + `"}}`)},
		{name: "nested fields home malformed", accounts: "alpha %SEAT%\n", seatJSON: both,
			homeJSON: []byte(`{"oauthAccount":{"emailAddress":"` + zadvW17EmailCanary + `","orgName":"` + zadvW17Canary + `","token":"` + zadvW17Canary + `"},"apiKey":"` + zadvW17Canary)},
		{name: "home json email wrong type", accounts: "alpha %SEAT%\n", seatJSON: both,
			homeJSON: []byte(`{"oauthAccount":{"emailAddress":["` + zadvW17EmailCanary + `"]}}`)},
		{name: "home json BOM and canary", accounts: "alpha %SEAT%\n", seatJSON: both, homeJSON: []byte(bomCanary)},
		{name: "home json NUL bytes", accounts: "alpha %SEAT%\n", seatJSON: both, homeJSON: []byte("{\x00" + zadvW17Canary + "\x00")},
		{name: "seat json invalid utf8 malformed", accounts: "alpha %SEAT%\n", seatJSON: []byte("\xff\xfe" + zadvW17Canary)},
		{name: "seat json huge malformed", accounts: "alpha %SEAT%\n", seatJSON: []byte("{" + zadvW17Canary + strings.Repeat("z", 1<<20))},
		{name: "accounts CRLF invalid token", accounts: zadvW17Canary + " %SEAT%\r\nalpha %SEAT%\r\n"},
		{name: "accounts BOM first token", accounts: bomCanary + " %SEAT%\n"},
		{name: "accounts NUL in token", accounts: "ca\x00" + zadvW17Canary + " %SEAT%\n"},
		{name: "accounts very long token", accounts: zadvW17Canary + strings.Repeat("q", 60000) + " %SEAT%\n"},
		{name: "accounts duplicate with canary dir", accounts: "alpha %SEAT%\nalpha %SEAT%/" + zadvW17Canary + "\n"},
		{name: "accounts relative dir canary", accounts: "alpha " + zadvW17Canary + "/rel\n"},
		{name: "accounts missing dir canary", accounts: "alpha /nonexistent/" + zadvW17Canary + "\n"},
		{name: "accounts dir with NUL", accounts: "alpha /nonexistent/\x00" + zadvW17Canary + "\n"},
		{name: "accounts tilde dir canary", accounts: "alpha ~/" + zadvW17Canary + "\n"},
		{name: "seat dir path carries canary, seat json dir", seatDir: ".seat-" + zadvW17Canary, accounts: "alpha %SEAT%\n", seatJSON: both,
			setup: func(t *testing.T, w zadvW17World) {
				if err := os.Remove(filepath.Join(w.seat, ".claude.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(w.seat, ".claude.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "seat dir path carries canary, seat json unreadable", seatDir: ".seat-" + zadvW17Canary, accounts: "alpha %SEAT%\n", seatJSON: both,
			setup: func(t *testing.T, w zadvW17World) {
				if os.Geteuid() == 0 {
					t.Skip("root reads everything")
				}
				if err := os.Chmod(filepath.Join(w.seat, ".claude.json"), 0); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "seat dir path carries canary, seat json symlink loop", seatDir: ".seat-" + zadvW17Canary, accounts: "alpha %SEAT%\n",
			setup: func(t *testing.T, w zadvW17World) {
				if err := os.Symlink(filepath.Join(w.seat, ".claude.json"), filepath.Join(w.seat, ".claude.json")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}},
		{name: "seat dir path carries canary, seat dir unreadable", seatDir: ".seat-" + zadvW17Canary, accounts: "alpha %SEAT%\n", seatJSON: both,
			setup: func(t *testing.T, w zadvW17World) {
				if os.Geteuid() == 0 {
					t.Skip("root reads everything")
				}
				if err := os.Chmod(w.seat, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(w.seat, 0o700) })
			}},
		{name: "seat dir path carries canary, home json malformed", seatDir: ".seat-" + zadvW17Canary, accounts: "alpha %SEAT%\n", seatJSON: both,
			homeJSON: []byte("{{{")},
		{name: "marker invalid token", accounts: "alpha %SEAT%\n", marker: []byte(zadvW17Canary + "\n")},
		{name: "marker BOM token", accounts: "alpha %SEAT%\n", marker: []byte(bomCanary + "\n")},
		{name: "marker oversized", accounts: "alpha %SEAT%\n", marker: []byte(zadvW17Canary + strings.Repeat("x", 9000))},
		{name: "marker NUL", accounts: "alpha %SEAT%\n", marker: []byte("al\x00" + zadvW17Canary)},
		{name: "marker invalid token, no accounts file", marker: []byte("../" + zadvW17Canary + "\n")},
		{name: "marker names unmapped seat", accounts: "alpha %SEAT%\n", marker: []byte("beta\n")},
		{name: "marker is a symlink to a canary file", accounts: "alpha %SEAT%\n",
			setup: func(t *testing.T, w zadvW17World) {
				target := filepath.Join(w.home, "target-"+"canary")
				zadvW17Write(t, target, []byte(zadvW17Canary))
				if err := os.Symlink(target, filepath.Join(w.checkout, ".claude-account")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}},
		{name: "marker unreadable", accounts: "alpha %SEAT%\n", marker: []byte(zadvW17Canary),
			setup: func(t *testing.T, w zadvW17World) {
				if os.Geteuid() == 0 {
					t.Skip("root reads everything")
				}
				if err := os.Chmod(filepath.Join(w.checkout, ".claude-account"), 0); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "accounts file is a directory", setup: func(t *testing.T, w zadvW17World) {
			if err := os.Mkdir(filepath.Join(w.home, ".claude-accounts"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "accounts file oversized with canary", accounts: zadvW17Canary + strings.Repeat(" ", 70000) + "\n"},
	}
	canaries := []string{zadvW17Canary, "canary:9c1d", "9c1d", zadvW17EmailCanary, "w17-canary", "leak.invalid", "target-canary"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dirName := c.seatDir
			if dirName == "" {
				dirName = ".claude-alpha"
			}
			w := zadvW17NewWorld(t, dirName)
			resolved, rErr := filepath.EvalSymlinks(w.seat)
			if rErr != nil {
				t.Fatal(rErr)
			}
			if c.accounts != "" {
				zadvW17Write(t, filepath.Join(w.home, ".claude-accounts"), []byte(strings.ReplaceAll(c.accounts, "%SEAT%", resolved)))
			}
			if c.seatJSON != nil {
				zadvW17Write(t, filepath.Join(w.seat, ".claude.json"), c.seatJSON)
			}
			if c.homeJSON != nil {
				zadvW17Write(t, filepath.Join(w.home, ".claude.json"), c.homeJSON)
			}
			if c.marker != nil {
				zadvW17Write(t, filepath.Join(w.checkout, ".claude-account"), c.marker)
			}
			if c.setup != nil {
				c.setup(t, w)
			}
			_, ovErr := claudeSeatOverrides(w.checkout)
			if ovErr == nil {
				return // nothing to leak: the launch was allowed
			}
			zadvW17AssertClean(t, ovErr, canaries...)
			s, startErr := StartClaude(t.Context(), Options{Program: bin, Cwd: w.checkout, Mode: "ask"})
			if s != nil {
				_, _ = s.Close()
				t.Fatalf("StartClaude started a process although the seat check refused: %v", ovErr)
			}
			if startErr == nil || !NoProcessStarted(startErr) {
				t.Fatalf("want a pre-spawn refusal, got %v", startErr)
			}
			zadvW17AssertClean(t, startErr, canaries...)
		})
	}
}

// Every refusal path of the seat code must be reached by at least some case of
// the table above; this one asserts the validated-name carve-out works and
// that a *valid* canary seat name is the only content an error may repeat.
func TestZadvW17ValidatedSeatNameMayAppearButNothingElse(t *testing.T) {
	w := zadvW17NewWorld(t, ".claude-alpha")
	zadvW17Write(t, filepath.Join(w.home, ".claude-accounts"), []byte("alpha "+w.seat+"\n"))
	zadvW17Write(t, filepath.Join(w.checkout, ".claude-account"), []byte("beta-valid.name extra-"+zadvW17Canary+" more\n"))
	_, err := claudeSeatOverrides(w.checkout)
	if err == nil {
		t.Fatal("unknown seat should be refused")
	}
	zadvW17AssertClean(t, err, "extra-", zadvW17Canary, "more")
}

// Concurrency under -race.
func TestZadvW17SeatResolutionIsRaceFree(t *testing.T) {
	w := zadvW17NewWorld(t, ".claude-alpha")
	zadvW17Write(t, filepath.Join(w.home, ".claude-accounts"), []byte("alpha "+w.seat+"\n"))
	zadvW17Write(t, filepath.Join(w.seat, ".claude.json"), zadvW17Email("a@x.invalid"))
	zadvW17Write(t, filepath.Join(w.home, ".claude.json"), zadvW17Email("b@x.invalid"))
	pinned := filepath.Join(w.home, "pinned")
	zadvW17Write(t, filepath.Join(pinned, ".claude-account"), []byte("alpha\n"))
	bad := filepath.Join(w.home, "bad")
	zadvW17Write(t, filepath.Join(bad, ".claude-account"), []byte(zadvW17Canary+"\n"))

	var wg sync.WaitGroup
	errs := make(chan string, 8*30)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if ov, err := claudeSeatOverrides(pinned); err != nil || zadvW17SeatDir(ov) == "" {
					errs <- fmt.Sprintf("pinned: ov=%+v err=%v", ov, err)
				}
				if _, err := claudeSeatOverrides(bad); err == nil || strings.Contains(err.Error(), zadvW17Canary) {
					errs <- fmt.Sprintf("bad marker: %v", err)
				}
				if ov, err := claudeSeatOverrides(w.checkout); err != nil || len(ov.set) != 0 {
					errs <- fmt.Sprintf("default: ov=%+v err=%v", ov, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func zadvW17SeatDir(o envOverrides) string {
	for _, kv := range o.set {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return v
		}
	}
	return ""
}

// Docstring: the invariants list must stay attached to claudeSeatOverrides.
// After the fix the new usableClaudeHome comment was spliced into the end of
// that block, which orphans claudeSeatOverrides and mis-documents the helper.
func TestZadvW17InvariantsDocstringStaysOnClaudeSeatOverrides(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "claude_seat.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			docs[fd.Name.Name] = fd.Doc.Text()
		}
	}
	if !strings.Contains(docs["claudeSeatOverrides"], "Invariants enforced before any child process starts") {
		t.Fatalf("claudeSeatOverrides lost its invariants docstring (doc=%q)", docs["claudeSeatOverrides"])
	}
	if strings.Contains(docs["usableClaudeHome"], "Invariants enforced") {
		t.Fatalf("usableClaudeHome carries the claudeSeatOverrides invariants block")
	}
	if !strings.HasPrefix(docs["claudeSeatOverrides"], "claudeSeatOverrides") {
		t.Fatalf("claudeSeatOverrides doc must start with its name (go doc convention): %.60q", docs["claudeSeatOverrides"])
	}
}
