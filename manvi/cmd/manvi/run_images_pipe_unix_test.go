//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The defect in --image on a pipe was blocking forever, not the pipe: a
// process substitution (`--image <(cat a.png)`, which is /dev/fd/N) or
// `cat a.png | manvi run --image /dev/stdin` has a live writer and worked
// before the regular-file check. A pipe is read, with a bound.

const pipeTestPNG = "\x89PNG\r\n\x1a\n" + "\x00\x00\x00\rIHDR" + "\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00"

// shortPipeWait shortens the wait for a pipe so the refusals are quick, and
// puts it back.
func shortPipeWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := imagePipeWait
	imagePipeWait = d
	t.Cleanup(func() { imagePipeWait = old })
}

func TestAnImageOnAPipeWithALiveWriterIsLoaded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	go func() {
		_, _ = w.WriteString(pipeTestPNG)
		_ = w.Close()
	}()
	// What a shell hands over for <(cat a.png) and for /dev/stdin.
	path := fmt.Sprintf("/dev/fd/%d", r.Fd())
	returned, block, err := loadBlockWithin(t, path, "")
	if !returned {
		t.Fatal("loadRunImage(/dev/fd/N) blocked")
	}
	if err != nil || block.MediaType != "image/png" || string(block.Data) != pipeTestPNG {
		t.Fatalf("a pipe with a live writer: type=%q len=%d err=%v, want the PNG loaded", block.MediaType, len(block.Data), err)
	}
}

func TestAnImageOnANamedPipeWhoseWriterArrivesLaterIsLoaded(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "img")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0) // #nosec G304 -- a FIFO this test made under t.TempDir
		if err != nil {
			return
		}
		_, _ = w.WriteString(pipeTestPNG)
		_ = w.Close()
	}()
	returned, block, err := loadBlockWithin(t, fifo, fifo)
	if !returned {
		t.Fatal("loadRunImage(FIFO) blocked")
	}
	if err != nil || block.MediaType != "image/png" {
		t.Fatalf("a named pipe whose writer starts late: type=%q err=%v, want the PNG loaded", block.MediaType, err)
	}
}

func TestAnImageOnANamedPipeWithNoWriterIsRefusedWithinTheBound(t *testing.T) {
	shortPipeWait(t, 400*time.Millisecond)
	fifo := filepath.Join(t.TempDir(), "img")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	returned, _, err := loadBlockWithin(t, fifo, fifo)
	if !returned {
		t.Fatal("loadRunImage(FIFO with no writer) blocked: no --timeout applies before the provider is built")
	}
	if !errors.Is(err, errImagePipeTimeout) {
		t.Fatalf("err = %v, want errImagePipeTimeout", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("a refusal that should take about 400ms took %s", took)
	}
}

func TestAnImageWriterThatStallsIsRefusedWithinTheBound(t *testing.T) {
	shortPipeWait(t, 400*time.Millisecond)
	fifo := filepath.Join(t.TempDir(), "img")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0) // #nosec G304 -- a FIFO this test made under t.TempDir
		if err != nil {
			return
		}
		defer func() { _ = w.Close() }()
		_, _ = w.WriteString(pipeTestPNG[:12]) // a start, then nothing, with the pipe held open
		<-release
	}()
	started := time.Now()
	returned, _, err := loadBlockWithin(t, fifo, "")
	if !returned {
		t.Fatal("loadRunImage(stalled writer) blocked")
	}
	if !errors.Is(err, errImagePipeTimeout) {
		t.Fatalf("err = %v, want errImagePipeTimeout", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("a refusal that should take about 400ms took %s", took)
	}
}

// A pipe is held to the same size as a file, and is not read past it.
func TestAnImagePipeOverTheSizeLimitIsRefused(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	go func() {
		chunk := []byte(strings.Repeat("\x00", 1<<20))
		_, _ = w.WriteString(pipeTestPNG)
		for i := 0; i < maxImageBytes>>20+2; i++ {
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		_ = w.Close()
	}()
	returned, _, err := loadBlockWithin(t, fmt.Sprintf("/dev/fd/%d", r.Fd()), "")
	if !returned {
		t.Fatal("loadRunImage(oversize pipe) blocked")
	}
	if err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("err = %v, want the 8 MiB refusal", err)
	}
}
