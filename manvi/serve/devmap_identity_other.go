//go:build !unix

package serve

import "os"

// identOf has no device and inode to read here, so the resolved path is the
// identity.
func identOf(_ os.FileInfo, canon string) rootIdent { return rootIdent{path: canon} }
