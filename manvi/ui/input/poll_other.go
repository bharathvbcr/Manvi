//go:build !unix

package input

import "io"

func newInputPoller(io.Reader) (inputPoller, io.Closer, error) {
	return nil, nil, nil
}
