package main

import "runtime"

// exe is the file name a stand-in tool needs on this platform. A program is
// found by its PATHEXT extension on Windows, so a planted or trusted "dcverify"
// with no extension is simply not there for the resolver, and a test of what the
// resolver trusts would pass without ever putting a candidate in front of it.
func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
