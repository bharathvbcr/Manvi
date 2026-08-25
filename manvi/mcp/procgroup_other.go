//go:build !unix

package mcp

import "os/exec"

// On platforms without process groups the server is still bounded by the
// direct-child kill in Close and by cmd.WaitDelay, which caps how long
// leftover descendants may hold the stdio pipes before Wait gives up on them.
//
// This file exists so the package compiles off unix at all. syscall.Kill and
// SysProcAttr.Setpgid do not exist on every GOOS, and referencing them from
// client.go made a leaf package with no other platform dependency refuse to
// build anywhere else — the same break devcouncil already solved with
// exec_unix.go / exec_other.go.
func setOwnProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {}
