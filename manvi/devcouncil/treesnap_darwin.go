//go:build darwin

package devcouncil

import (
	"io/fs"
	"syscall"
)

// lstatStamp reads the stamp of one path with a single lstat and no allocation
// beyond the stat buffer: it is called once per listed file, twice per search.
func lstatStamp(name string) (fsStamp, bool) {
	var st syscall.Stat_t
	if err := syscall.Lstat(name, &st); err != nil {
		return fsStamp{}, false
	}
	//nolint:unconvert,gosec // field widths differ between platforms; the device number is only compared, never ordered.
	return fsStamp{
		dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint64(st.Nlink),
		mode: uint32(st.Mode), mtime: st.Mtimespec.Nano(), ctime: st.Ctimespec.Nano(),
	}, true
}

// stampOfInfo is the stamp inside an lstat result already in hand.
func stampOfInfo(fi fs.FileInfo) (fsStamp, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fsStamp{}, false
	}
	//nolint:unconvert,gosec // field widths differ between platforms; the device number is only compared, never ordered.
	return fsStamp{
		dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint64(st.Nlink),
		mode: uint32(st.Mode), mtime: st.Mtimespec.Nano(), ctime: st.Ctimespec.Nano(),
	}, true
}
