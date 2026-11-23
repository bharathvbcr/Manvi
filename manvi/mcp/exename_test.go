package mcp

import "runtime"

// exeName is the file name a program built for this platform needs: Windows
// will only start a file that has an executable extension, so a stand-in server
// built as "fakeserver" is "executable file not found" there.
func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
