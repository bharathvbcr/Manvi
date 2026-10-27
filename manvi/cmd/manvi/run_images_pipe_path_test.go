package main

import "testing"

// The Windows build refuses a named-pipe path by its name, because inspecting
// one connects to it. The predicate is plain string work and is checked on
// every platform; the refusal itself runs only in the Windows build.
func TestWindowsNamedPipePathsAreRecognisedByName(t *testing.T) {
	for path, want := range map[string]bool{
		`\\.\pipe\img`:          true,
		`\\.\PIPE\img`:          true,
		`//./pipe/img`:          true,
		`\\?\pipe\img`:          true,
		`\\?\C:\images\a.png`:   false,
		`C:\images\pipe\a.png`:  false,
		`\\.\C:\images\a.png`:   false,
		`\\server\share\pipe\x`: false,
		`images/pipe/a.png`:     false,
		`/dev/fd/3`:             false,
		`\\.\pipeline\a.png`:    false,
		`pipe`:                  false,
		``:                      false,
	} {
		if got := windowsPipePath(path); got != want {
			t.Errorf("windowsPipePath(%q) = %v, want %v", path, got, want)
		}
	}
}
