//go:build darwin || linux || freebsd || netbsd || openbsd

package credentials

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// probeLockedMemory locks and releases pages pages of anonymous memory, which
// is the operation memguard is about to attempt. An error means memguard's
// would fail too.
func probeLockedMemory(pages int) error {
	region, err := unix.Mmap(-1, 0, pages*os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	lockErr := unix.Mlock(region)
	if lockErr != nil {
		lockErr = fmt.Errorf("mlock: %w", lockErr)
	} else if err := unix.Munlock(region); err != nil {
		lockErr = fmt.Errorf("munlock: %w", err)
	}
	if err := unix.Munmap(region); err != nil {
		return errors.Join(lockErr, fmt.Errorf("munmap: %w", err))
	}
	return lockErr
}
