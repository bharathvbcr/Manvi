//go:build !unix

package term

import "errors"

// state is a placeholder on platforms this package cannot drive.
type state struct{}

// errUnsupported is returned rather than a silent degradation to a
// line-buffered mode. A TUI that "starts" without raw mode does not work — it
// echoes keystrokes over its own frame and never sees a cursor key — and
// presenting that as a running UI wastes more of an operator's time than
// refusing does.
var errUnsupported = errors.New("term: raw mode is not implemented on this platform; use the line renderer (manvi watch)")

func getState(fd uintptr) (*state, error) { return nil, errUnsupported }

func setState(fd uintptr, s *state) error { return errUnsupported }

func makeRaw(fd uintptr) (*state, error) { return nil, errUnsupported }

func size(fd uintptr) (int, int, error) { return 0, 0, errUnsupported }
