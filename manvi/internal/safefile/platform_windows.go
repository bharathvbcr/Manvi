package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// Go 1.26 forwards FILE_FLAG_OPEN_REPARSE_POINT in both os.OpenFile and
// os.Root.OpenFile. It opens the link itself instead of its target. Inspect
// that pinned handle before permitting content reads or writes.
const noFollowFlags = syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.O_CLOEXEC

// Root.Stat captures identity from a handle immediately. Ordinary Windows
// os.Stat can defer identity retrieval to a later exclusive-share CreateFile,
// which would both re-resolve the old path and conflict with our held roots.
func pathStat(name string, follow bool) (info fs.FileInfo, result error) {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if follow {
		return root.Stat(filepath.Base(name))
	}
	return root.Lstat(filepath.Base(name))
}

func Stat(name string) (fs.FileInfo, error)  { return pathStat(name, true) }
func Lstat(name string) (fs.FileInfo, error) { return pathStat(name, false) }

func handleInfo(f *os.File) (syscall.ByHandleFileInformation, error) {
	var info syscall.ByHandleFileInformation
	err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info)
	runtime.KeepAlive(f)
	return info, err
}

func validateHandle(f *os.File) error {
	info, err := handleInfo(f)
	if err != nil {
		return err
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("refusing a reparse point")
	}
	kind, err := syscall.GetFileType(syscall.Handle(f.Fd()))
	runtime.KeepAlive(f)
	if err != nil {
		return err
	}
	if kind != syscall.FILE_TYPE_DISK {
		return errors.New("not a disk file")
	}
	return nil
}

// LinkCount uses the same open handle used for I/O; Win32FileAttributeData
// returned by FileInfo.Sys does not contain the number of hard links.
func LinkCount(f *os.File) (uint64, error) {
	info, err := handleInfo(f)
	return uint64(info.NumberOfLinks), err
}
