//go:build !(darwin || linux || freebsd || netbsd || openbsd)

package credentials

// probeLockedMemory has no probe on this platform, so a failure there is caught
// only by guarded's bounded wait: a deadlocked call is reported after
// memguardBound rather than before memguard is called.
func probeLockedMemory(int) error { return nil }
