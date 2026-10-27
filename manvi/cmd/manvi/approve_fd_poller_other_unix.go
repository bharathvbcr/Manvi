//go:build unix && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !linux

package main

// isRuntimePoller is false where the runtime's poller is not a descriptor this
// package knows how to recognise (event ports, for one).
func isRuntimePoller(int) bool { return false }
