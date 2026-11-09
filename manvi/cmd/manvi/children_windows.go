//go:build !unix

package main

// endLeftoverChildren is a no-op off unix: there is no process table to list
// without a Job Object, and none of the children the TUI starts is placed in
// one. See children_unix.go for what it does where there is.
func endLeftoverChildren() []int { return nil }
