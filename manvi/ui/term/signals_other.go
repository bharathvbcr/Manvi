//go:build !unix

package term

import "os"

// The platform has no resize signal; size changes are discovered by polling.
func resizeSignal() os.Signal { return nil }

func suspendSignals() (stop, cont os.Signal) { return nil, nil }

func raiseSelf(sig os.Signal) error { return nil }
