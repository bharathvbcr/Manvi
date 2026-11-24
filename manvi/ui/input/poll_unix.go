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
	fd := int32(rawFd)

	// Probe with zero timeout first: devices such as /dev/tty on macOS return
	// POLLNVAL from poll(2). Fall back to non-cancellable reads in that case.
	probe := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(probe, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || probe[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
			return nil, nil, nil
		}
		break
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
		fd:     fd,
		wakeFd: int32(rawWakeFd),
		wakeR:  wakeR,
		wake:   wc,
	}, wc, nil
}

func (p *unixPoller) waitReadable(done <-chan struct{}) pollResult {
	fds := []unix.PollFd{
		{Fd: p.fd, Events: unix.POLLIN},
		{Fd: p.wakeFd, Events: unix.POLLIN},
	}
	for {
		select {
		case <-done:
			return pollStopped
		default:
		}
		fds[0].Revents = 0
		fds[1].Revents = 0
		n, err := unix.Poll(fds, -1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return pollFallback
		}
		if n == 0 {
			continue
		}
		if fds[1].Revents != 0 {
			return pollStopped
		}
		select {
		case <-done:
			return pollStopped
		default:
		}
		if fds[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
			return pollFallback
		}
		if fds[0].Revents != 0 {
			return pollReady
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
