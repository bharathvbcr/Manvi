//go:build !unix

package devcouncil

import "errors"

// runMkfifo reports that fifos cannot be built here, so the tests that need one
// skip.
func runMkfifo(path string, mode uint32) error {
	return errors.New("fifos are not available on this platform")
}
