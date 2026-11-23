package codingagent

import "runtime"

// exeName is the file name a stand-in program needs on this platform. Windows
// resolves a program by its PATHEXT extension, so a fixture called "claude" is
// "executable file not found" there before the launch check ever reads it. The
// check judges a file by its content (an image or a #! line), not its name, so
// a stand-in named "claude.exe" is judged exactly as one named "claude" is.
func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
