package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// Doctor on a state directory that is a link must not create, write or open a
// store through it, in every shape of link.
func TestZadvW24DoctorTouchesNothingThroughAnyShapeOfLinkedStateDir(t *testing.T) {
	cases := map[string]func(t *testing.T, root, outside string){
		"default state dir is a link": func(t *testing.T, root, outside string) {
			if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
				t.Skip(err)
			}
		},
		"leaf link": func(t *testing.T, root, outside string) {
			if err := os.Mkdir(filepath.Join(root, "a"), 0o755); err != nil { // #nosec G301 -- a directory under this test's own t.TempDir
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "a", "state")); err != nil {
				t.Skip(err)
			}
			t.Setenv("MANVI_STATE_DIR", "a/state")
		},
		"middle link": func(t *testing.T, root, outside string) {
			if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
				t.Skip(err)
			}
			t.Setenv("MANVI_STATE_DIR", "a/b/c")
		},
		"dangling link": func(t *testing.T, root, outside string) {
			if err := os.Symlink(filepath.Join(outside, "absent"), filepath.Join(root, "a")); err != nil {
				t.Skip(err)
			}
			t.Setenv("MANVI_STATE_DIR", "a/state")
		},
		"store db path crosses a link, state dir fine": func(t *testing.T, root, outside string) {
			if err := os.Symlink(outside, filepath.Join(root, "dblink")); err != nil {
				t.Skip(err)
			}
			t.Setenv("MANVI_STORE_DB", filepath.Join(root, "dblink", "store.sqlite"))
		},
		"graph path crosses a link, state dir fine": func(t *testing.T, root, outside string) {
			if err := os.Symlink(outside, filepath.Join(root, "glink")); err != nil {
				t.Skip(err)
			}
			t.Setenv("MANVI_GRAPH", filepath.Join(root, "glink", "g.json"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			root, outside := stateTree(t)
			defer setProjectRootForTest(root)()
			t.Chdir(root)
			t.Setenv("MANVI_STATE_DIR", "")
			t.Setenv("MANVI_STORE_DB", "")
			t.Setenv("MANVI_GRAPH", "")
			setup(t, root, outside)
			var out, notes bytes.Buffer
			if err := run(&out, &notes, []string{"doctor"}); err != nil {
				t.Fatalf("doctor aborted: %v", err)
			}
			for _, l := range strings.Split(out.String(), "\n") {
				if strings.Contains(l, "store ") || strings.Contains(l, "UNAVAILABLE") || strings.Contains(l, "REFUSED") || strings.Contains(l, "reachable") {
					t.Log(strings.TrimSpace(l))
				}
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("doctor created %v outside the project\n%s", entries, out.String())
			}
		})
	}
}

// Doctor's exemption is for doctor only: no other first argument, spelled any way,
// reaches the diagnose-anyway path.
func TestZadvW24OnlyDoctorIsExemptFromTheStateDirRefusal(t *testing.T) {
	root, outside := stateTree(t)
	defer setProjectRootForTest(root)()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	t.Setenv("MANVI_STATE_DIR", "link/state")
	for _, args := range [][]string{{"Doctor"}, {"doctor-x"}, {"help", "doctor"}, {"--", "doctor"}, {"lease", "doctor"}, {"reflect", "doctor"}} {
		err := run(io.Discard, io.Discard, args)
		if err == nil {
			continue // a command that does not prepare the repository does not reach it either
		}
		if strings.Contains(err.Error(), "doctor") && strings.Contains(err.Error(), "unknown") {
			continue
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was created outside the project: %v", entries)
	}
}

func zadvW24HostileManager(t *testing.T) (root string, mgr *mcp.Manager) {
	t.Helper()
	base := t.TempDir()
	// a repository directory whose own name carries an escape sequence, a bidi
	// override and a newline
	root = filepath.Join(base, "rep\x1b[2Jo\u202e\nmanvi: mcp: forged")
	if err := os.Mkdir(root, 0o755); err != nil { // #nosec G301 -- a directory under this test's own t.TempDir
		t.Skipf("file system refuses the hostile directory name: %v", err)
	}
	name := "dup\u001b]0;pwn\u0007\u202e\nmanvi: mcp: ok"
	body := `{"mcpServers":{"` + strings.NewReplacer("\n", "\\n", "\x1b", "\\u001b", "\u0007", "\\u0007").Replace(name) + `":{"command":"a"},"":{"command":"c"}}}`
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	alt := strings.Replace(body, `"command":"a"`, `"command":"b"`, 1)
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(alt), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := buildMCP(newTestRegistry(t), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, mgr
}

func zadvW24AssertInert(t *testing.T, label, text string, wantPrefix string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(line), wantPrefix) && !strings.HasPrefix(line, "                  ") {
			t.Errorf("%s: a line that does not start as a notice (forged?): %q", label, line)
		}
	}
	for _, r := range strings.TrimSuffix(text, "\n") {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e || r == 0x2028 || r == 0x2029) {
			t.Errorf("%s: live %U in %q", label, r, text)
			return
		}
	}
}

func TestZadvW24HostileNamesAndPathsCannotForgeOrRepaintMCPNotices(t *testing.T) {
	_, mgr := zadvW24HostileManager(t)
	if len(mgr.Skipped()) == 0 {
		t.Skip("no refusal was produced")
	}
	var boot bytes.Buffer
	reportMCPRefusals(&boot, mgr)
	zadvW24AssertInert(t, "boot", boot.String(), "manvi: mcp:")
	if boot.Len() == 0 {
		t.Log("boot printed nothing (already reported in this process)")
	}
	lines, _ := mcpRefusalLines(mgr.Skipped(), 32)
	for _, l := range lines {
		if strings.ContainsAny(l, "\n\r\x1b") {
			t.Errorf("a notice line carries a line break or escape: %q", l)
		}
	}
}

// The exact bounds, with the omitted count said exactly and never as "+0".
func TestZadvW24NoticeBoundsAreExact(t *testing.T) {
	mk := func(n int) []mcp.SkippedManifest {
		var s []mcp.SkippedManifest
		for i := 0; i < n; i++ {
			s = append(s, mcp.SkippedManifest{Path: "p" + string(rune('A'+i%26)) + string(rune('a'+i/26)), Reason: "r"})
		}
		return s
	}
	for _, c := range []struct{ n, limit, wantLines, wantMore int }{
		{0, 8, 0, 0}, {7, 8, 7, 0}, {8, 8, 8, 0}, {9, 8, 8, 1}, {32, 32, 32, 0}, {33, 32, 32, 1}, {100, 32, 32, 68},
	} {
		lines, more := mcpRefusalLines(mk(c.n), c.limit)
		if len(lines) != c.wantLines || more != c.wantMore {
			t.Errorf("n=%d limit=%d: %d lines, %d more; want %d, %d", c.n, c.limit, len(lines), more, c.wantLines, c.wantMore)
		}
	}
	// once per process: a second report of a superset prints only the new entries
	first := mk(10)
	for i := range first {
		first[i].Path = "bounds-" + first[i].Path
	}
	if got := unreported(first); len(got) != 10 {
		t.Fatalf("first report: %d fresh, want 10", len(got))
	}
	more := append(append([]mcp.SkippedManifest{}, first...), mcp.SkippedManifest{Path: "bounds-new", Reason: "r"})
	if got := unreported(more); len(got) != 1 || got[0].Path != "bounds-new" {
		t.Fatalf("a re-discovery reported %v, want only the new one", got)
	}
}

// Notices when stderr is closed or absent do not panic.
func TestZadvW24NoticesSurviveAClosedOrNilStderr(t *testing.T) {
	_, mgr := zadvW24HostileManager(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_ = w.Close()
	reportMCPRefusals(w, mgr) // closed
	var nilFile *os.File
	reportMCPRefusals(nilFile, mgr) // a nil *os.File inside a non-nil io.Writer
	reportMCPRefusals(nil, mgr)
	reportMCPRefusals(io.Discard, nil)
}
