//go:build !linux && !darwin

package treefile

import "os"

// renameNoReplace has no atomic no-replace rename on this platform, so Publish
// falls back to linking the stage onto the name.
func renameNoReplace(*os.Root, string, string) error { return errNoAtomicRename }
