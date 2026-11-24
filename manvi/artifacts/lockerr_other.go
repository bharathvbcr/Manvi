//go:build !windows

package artifacts

// lockHeldElsewhere reports an error from creating the lock file that means the
// name is taken for a moment. Off Windows, O_EXCL answers EEXIST for every such
// case and a denial is a real one, so nothing but ErrExist is a contender.
func lockHeldElsewhere(error) bool { return false }
