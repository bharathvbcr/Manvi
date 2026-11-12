//go:build !unix

package storeguard

// reapLeaked reports nothing: the process listing the guard reads is unix
// only. The cleanup NewClient registers still closes every client.
func reapLeaked() []child { return nil }
