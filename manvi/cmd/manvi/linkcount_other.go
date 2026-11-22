//go:build !unix

package main

import "os"

// userConfigLinkCount reports a file that exists as having one link: this
// platform does not expose a link count through os.FileInfo, and the file's
// own path is what refuseUserConfigInside has already checked.
func userConfigLinkCount(path string) (uint64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	return 1, nil
}
