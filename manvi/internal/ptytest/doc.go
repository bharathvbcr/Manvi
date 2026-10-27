// Package ptytest opens pseudo-terminals for tests.
//
// A pseudo-terminal is the only way to test anything that is about a tty: raw
// mode and its restoration, window size, the escape sequences a full-screen
// program writes and takes back, and what a signal does to a program that holds
// the terminal. None of it can be observed through a pipe, because a pipe is
// not a terminal and the programs under test correctly refuse it.
//
// The ioctl numbers are platform constants rather than a portable API. They
// were verified by running them on each platform, not recalled; a wrong one
// surfaces as a failure to open, which fails the test rather than skipping it.
// It builds on Linux and the BSDs (macOS included) and is empty elsewhere, so a
// test that imports it carries a unix build constraint of its own.
package ptytest
