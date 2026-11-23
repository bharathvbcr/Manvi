//go:build unix

package input

import (
	"errors"
	"io"
	"math"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type fdSource interface {
	Fd() uintptr
}

type inputPoller interface {
	waitReadable(done <-chan struct{}) bool
	close()
}

type wakeCloser struct {
	once sync.Once
	w    *os.File
}

func (wc *wakeCloser) Close() error {
	if wc == nil {
		return nil
	}
	var err error
	wc.once.Do(func() {
		if wc.w != nil {
			_, _ = wc.w.Write([]byte{0})
			err = wc.w.Close()
		}
	})
	return err
}

type unixPoller struct {
	fd     int32
	wakeFd int32
	wakeR  *os.File
	wake   *wakeCloser
}

func newInputPoller(src io.Reader) (inputPoller, io.Closer, error) {
	fs, ok := src.(fdSource)
	if !ok {
		return nil, nil, nil
	}
	rawFd := fs.Fd()
	if rawFd > math.MaxInt32 {
		return nil, nil, nil
	}
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	rawWakeFd := wakeR.Fd()
	if rawWakeFd > math.MaxInt32 {
		_ = wakeR.Close()
		_ = wakeW.Close()
		return nil, nil, nil
	}
	wc := &wakeCloser{w: wakeW}
	return &unixPoller{
		fd:     int32(rawFd),
		wakeFd: int32(rawWakeFd),
		wakeR:  wakeR,
		wake:   wc,
	}, wc, nil
}

func (p *unixPoller) waitReadable(done <-chan struct{}) bool {
	fds := []unix.PollFd{
		{Fd: p.fd, Events: unix.POLLIN},
		{Fd: p.wakeFd, Events: unix.POLLIN},
	}
	for {
		select {
		case <-done:
			return false
		default:
		}
		fds[0].Revents = 0
		fds[1].Revents = 0
		n, err := unix.Poll(fds, -1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return false
		}
		if n == 0 {
			continue
		}
		if fds[1].Revents != 0 {
			return false
		}
		select {
		case <-done:
			return false
		default:
		}
		if fds[0].Revents != 0 {
			return true
		}
	}
}

func (p *unixPoller) close() {
	if p == nil {
		return
	}
	_ = p.wake.Close()
	if p.wakeR != nil {
		_ = p.wakeR.Close()
	}
}
