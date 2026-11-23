// Package rootedpath names the one path shape that filepath.IsAbs misjudges on
// Windows, so every caller that sorts paths into "absolute" and "relative to the
// project" sorts it the same way.
package rootedpath

import (
	"path/filepath"
	"strings"
)

// NotAbsolute reports a path that names a root or a drive without being
// absolute. On Windows "/etc/passwd", `\x` and "C:foo" are that: filepath.IsAbs
// says no to all three, so a caller that reads "not absolute" as "relative to
// the project" joins them under a directory it trusts, and judges a file inside
// the repository that the kernel opens elsewhere. That is the permissive answer
// from a gate whose job is to fail closed. Such a path must be handed on as
// written (a gate refuses it as outside the root) or treated as pointing
// outside, never joined. Where a leading slash is absolute (every other
// platform) this is always false.
func NotAbsolute(p string) bool {
	if filepath.IsAbs(p) {
		return false
	}
	return filepath.VolumeName(p) != "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}
