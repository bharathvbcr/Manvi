//go:build !darwin && !linux && !windows

package budget

// processZombie cannot be told on this platform.
func processZombie(int) bool { return false }

// processStartUnix has no start time to offer on this platform, so a pid is
// never judged reused here.
func processStartUnix(int) (int64, bool) { return 0, false }
