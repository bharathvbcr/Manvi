//go:build windows

package devcouncil

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// realDir is the directory path names with every symbolic link and junction
// followed. The crates are linked into DevCouncil by directory junctions on a
// Windows checkout (a symlink needs a privilege a runner may not grant), and
// filepath.EvalSymlinks no longer follows a junction since Go 1.23, so it would
// report a crate that is linked correctly as a plain directory. The final path
// the OS reports for an open handle is where the junction really leads.
func realDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	name, err := windows.UTF16PtrFromString(resolved)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	final := windows.UTF16ToString(buf[:n])
	final = strings.TrimPrefix(final, `\\?\`)
	return filepath.Clean(final), nil
}
