//go:build !linux && !darwin

package treefile

import "os"

// renameNoReplace has no atomic no-replace rename on this platform, so Publish
// falls back to linking the stage onto the name.
// hasAtomicRename says Publish renames without replacing, so a published entry
// never has a second link. See stillLinked.
const hasAtomicRename = false

func renameNoReplace(*os.Root, string, string) error { return errNoAtomicRename }
