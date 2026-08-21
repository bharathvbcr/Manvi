//go:build !unix

package testsupport

// lockBuildOutputs is a no-op where flock is not available.
//
// Without it, two test processes can copy the cargo artifact while a third is
// replacing it. That is not silently tolerated: readStableArtifact re-checks
// the file's size against the bytes it actually read and retries when they
// disagree, so a torn read is caught and repeated rather than executed. The
// lock is what makes the copy contention-free; the size check is what makes it
// correct without one.
func lockBuildOutputs(string) (func(), error) { return func() {}, nil }
