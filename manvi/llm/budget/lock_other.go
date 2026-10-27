//go:build !unix && !windows

package budget

// processGone never claims a process is gone where it cannot check.
func processGone(int) bool { return false }
