package rootedpath

import (
	"runtime"
	"testing"
)

func TestNotAbsoluteIsTrueOnlyForARootedPathThatIsNotAbsolute(t *testing.T) {
	for _, p := range []string{"", "rel/x", "./a", "a/../b"} {
		if NotAbsolute(p) {
			t.Errorf("NotAbsolute(%q) = true, want false for a plain relative path", p)
		}
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{"/etc/passwd", `\x`, "C:foo", "D:bar/baz"} {
			if !NotAbsolute(p) {
				t.Errorf("NotAbsolute(%q) = false on windows, want true", p)
			}
		}
		if NotAbsolute(`C:\x`) {
			t.Error(`NotAbsolute("C:\x") = true, want false for an absolute path`)
		}
		return
	}
	// Where a leading slash is absolute nothing is rooted without being absolute.
	for _, p := range []string{"/etc/passwd", "/tmp/x"} {
		if NotAbsolute(p) {
			t.Errorf("NotAbsolute(%q) = true, want false where / alone is absolute", p)
		}
	}
}
