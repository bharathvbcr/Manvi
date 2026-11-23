package devcouncil

import "runtime"

// exeName is the file name a program built for this platform needs: Windows
// starts a program by its extension, so a stand-in built as "stub" is
// "executable file not found" there and a test that asserts it was never started
// passes without the program ever having been startable.
func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
