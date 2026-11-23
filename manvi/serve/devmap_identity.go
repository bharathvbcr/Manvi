package serve

// rootIdent names a directory by what it is rather than how it was spelled.
// On unix it is the device and inode, so a case variant or a second path to the
// same directory is one pool entry, and a directory deleted and recreated at
// the same path is a different one. Where the platform gives no such identity
// (devmap_identity_other.go) it falls back to the resolved path.
type rootIdent struct {
	dev, ino uint64
	path     string
}
