//go:build !unix

package input

import "io"

type inputPoller interface {
	waitReadable(done <-chan struct{}) bool
	close()
}

func newInputPoller(io.Reader) (inputPoller, io.Closer, error) {
	return nil, nil, nil
}
