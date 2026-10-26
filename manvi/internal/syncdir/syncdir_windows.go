//go:build windows

package syncdir

import "os"

// supported is false: a directory cannot be flushed on Windows.
const supported = false

// flush is a no-op. A directory handle cannot be flushed there:
// FlushFileBuffers needs write access that a directory handle opened by
// os.Open does not have, and NTFS journals the rename's metadata itself.
// Calling the Unix version would fail every write after it had been published.
// This reason was first recorded in llm/budget/lock_windows.go; it has not been
// exercised on a Windows CI run, only compiled for the target.
func flush(*os.File) error { return nil }
