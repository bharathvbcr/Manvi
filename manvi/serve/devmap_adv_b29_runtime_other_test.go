//go:build !unix

package serve

func syscallAlive(int) bool { return false }

func syscallKill(int) {}
