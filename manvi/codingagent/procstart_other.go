//go:build !linux && !darwin

package codingagent

func processStart(int) (int64, error) { return 0, ErrProcessStartUnsupported }

func processZombie(int) (bool, error) { return false, ErrProcessStartUnsupported }
