//go:build !unix

package treefile

import "io/fs"

// nameLinks reports nothing here: the caller counts the handle again after it
// looks the name up.
func nameLinks(fs.FileInfo) (uint64, bool) { return 0, false }
