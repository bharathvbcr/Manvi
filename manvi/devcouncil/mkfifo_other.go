//go:build !unix

package devcouncil

import "errors"

func runMkfifo(path string, mode uint32) error {
	return errors.New("fifos are not available on this platform")
}
