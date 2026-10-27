//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// loadBlockWithin runs loadRunImage and gives up waiting after a bound, because
// the failure under test is the call never returning. A call still blocked on a
// FIFO when the bound passes is released by opening the other end, so the
// goroutine does not outlive the test.
func loadBlockWithin(t *testing.T, path string, release string) (returned bool, block llm.ImageBlock, err error) {
	t.Helper()
	type result struct {
		block llm.ImageBlock
		err   error
	}
	done := make(chan result, 1)
	go func() { b, e := loadRunImage(path); done <- result{b, e} }()
	select {
	case got := <-done:
		return true, got.block, got.err
	case <-time.After(3 * time.Second):
		if release != "" {
			if w, openErr := os.OpenFile(release, os.O_WRONLY|syscall.O_NONBLOCK, 0); openErr == nil { // #nosec G304 -- a FIFO this test made under t.TempDir
				_ = w.Close()
			}
			<-done
		}
		return false, llm.ImageBlock{}, nil
	}
}

func loadWithin(t *testing.T, path string, release string) (returned bool, err error) {
	t.Helper()
	returned, _, err = loadBlockWithin(t, path, release)
	return returned, err
}

// A symbolic link is followed, as it always was: the operator named the path
// and a link to an image is an image. What it leads to decides, so a link to a
// FIFO nobody writes to times out and a link to a regular image still loads.
func TestAnImageSymlinkIsFollowedAndWhatItLeadsToDecides(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "real.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"+strings.Repeat("\x00", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(png, link); err != nil {
		t.Fatal(err)
	}
	block, err := loadRunImage(link)
	if err != nil || block.MediaType != "image/png" {
		t.Fatalf("a symlink to a regular PNG: block=%q err=%v, want it loaded as image/png", block.MediaType, err)
	}

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoLink := filepath.Join(dir, "pipelink.png")
	if err := os.Symlink(fifo, fifoLink); err != nil {
		t.Fatal(err)
	}
	shortPipeWait(t, 300*time.Millisecond)
	returned, err := loadWithin(t, fifoLink, fifo)
	if !returned {
		t.Fatal("loadRunImage(symlink to FIFO) still blocked after 3s")
	}
	if !errors.Is(err, errImagePipeTimeout) {
		t.Fatalf("a symlink to a FIFO with no writer: err = %v, want errImagePipeTimeout", err)
	}
}

func TestAnImagePathThatIsADirectoryIsRefused(t *testing.T) {
	returned, err := loadWithin(t, t.TempDir(), "")
	if !returned {
		t.Fatal("loadRunImage(directory) did not return")
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file (a directory)") {
		t.Fatalf("err = %v, want a directory refusal", err)
	}
}

func TestAnImagePathThatIsADeviceIsRefused(t *testing.T) {
	returned, err := loadWithin(t, os.DevNull, "")
	if !returned {
		t.Fatal("loadRunImage(/dev/null) did not return")
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a not-a-regular-file refusal", err)
	}
}

// The handle check is what holds when the name was a regular file at the first
// look and a FIFO by the time of the open: the open must not wait for a writer,
// and what it returns must not pass for a regular file.
func TestTheImageOpenDoesNotWaitForAWriterOnAFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "img")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	type opened struct {
		f   *os.File
		err error
	}
	done := make(chan opened, 1)
	go func() { f, err := openRunImage(fifo); done <- opened{f, err} }()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer func() { _ = got.f.Close() }()
		info, err := got.f.Stat()
		if err != nil || info.Mode().IsRegular() {
			t.Fatalf("the handle's mode = %v (err %v), want a non-regular file", info.Mode(), err)
		}
	case <-time.After(3 * time.Second):
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { // #nosec G304 -- a FIFO this test made under t.TempDir
			_ = w.Close()
		}
		t.Fatal("openRunImage(FIFO) blocked waiting for a writer")
	}
}
