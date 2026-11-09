//go:build !linux && !darwin

package devcouncil

import "io/fs"

// lstatStamp has no stamp on this platform, so no snapshot is ever usable and
// the full relist-and-judge path always runs: slower, never less safe.
func lstatStamp(string) (fsStamp, bool) { return fsStamp{}, false }

func stampOfInfo(fs.FileInfo) (fsStamp, bool) { return fsStamp{}, false }
