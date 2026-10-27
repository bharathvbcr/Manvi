package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

const (
	maxRunImages  = 4
	maxImageBytes = 8 << 20
)

// loadRunImages reads every --image path. The count and the size are refused
// before the bytes are kept, and the type is the sniffed content type, not the
// name. An empty list is not an error.
func loadRunImages(paths []string) ([]llm.ImageBlock, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > maxRunImages {
		return nil, fmt.Errorf("--image: at most %d images, got %d", maxRunImages, len(paths))
	}
	out := make([]llm.ImageBlock, 0, len(paths))
	for _, path := range paths {
		block, err := loadRunImage(path)
		if err != nil {
			return nil, err
		}
		out = append(out, block)
	}
	return out, nil
}

// loadRunImage reads one --image path. It runs while the arguments are parsed,
// before the --timeout context exists, so nothing here may wait on the path
// without a bound. A regular file is read as it always was (a symbolic link is
// followed, and what it leads to decides). A pipe is read too, because a
// process substitution (`--image <(cat a.png)`, /dev/fd/N) and
// `cat a.png | manvi run --image /dev/stdin` are pipes with a live writer: the
// defect was waiting forever on one with none, not the pipe. Devices, sockets
// and directories are refused. The kind is checked twice, by name and on the
// opened handle, which is the check that holds if the name was swapped between
// the two; the open does not wait for a writer, and a pipe is read for at most
// imagePipeWait and maxImageBytes.
func loadRunImage(path string) (block llm.ImageBlock, err error) {
	if err := refuseImagePath(path); err != nil {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %w", path, err)
	}
	if named, statErr := os.Stat(path); statErr == nil && !imageSourceAllowed(named.Mode()) {
		return llm.ImageBlock{}, notRegularImage(path, named.Mode())
	}
	f, err := openRunImage(path)
	if err != nil {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			block, err = llm.ImageBlock{}, fmt.Errorf("--image %s: %w", path, closeErr)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %w", path, err)
	}
	if !imageSourceAllowed(info.Mode()) {
		return llm.ImageBlock{}, notRegularImage(path, info.Mode())
	}
	var data []byte
	if info.Mode().IsRegular() {
		if info.Size() > maxImageBytes {
			return llm.ImageBlock{}, fmt.Errorf("--image %s: %d bytes exceeds the 8 MiB limit", path, info.Size())
		}
		data, err = io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	} else {
		data, err = readImagePipe(f, imagePipeWait)
	}
	switch {
	case errors.Is(err, errImagePipeTimeout):
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %w (%s)", path, err, imagePipeWait)
	case err != nil:
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %w", path, err)
	}
	if len(data) > maxImageBytes {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: exceeds the 8 MiB limit", path)
	}
	media, ok := sniffedImageType(data)
	if !ok {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: content type %s is not png, jpeg, gif or webp", path, media)
	}
	return llm.ImageBlock{MediaType: media, Data: data}, nil
}

// imagePipeWait bounds how long an --image pipe is waited on for its data, a
// var so a test can shorten it.
var imagePipeWait = 5 * time.Second

// errImagePipeTimeout is a pipe that produced no image within imagePipeWait: a
// FIFO nobody writes to, or a writer that started and stopped sending.
var errImagePipeTimeout = errors.New("no image data arrived on the pipe in time (no writer, or one that stopped sending)")

// windowsPipePath reports whether path names a Windows named pipe. Opening or
// even inspecting `\\.\pipe\x` connects to it, and nothing bounds that, so
// the Windows build refuses these paths before touching them.
func windowsPipePath(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasPrefix(p, `\\.\pipe\`) || strings.HasPrefix(p, `\\?\pipe\`)
}

// notRegularImage is the refusal for a path that is not a file of bytes: it
// names what the path is, because "not a regular file" alone does not tell an
// operator that the path they typed is a pipe.
func notRegularImage(path string, mode fs.FileMode) error {
	kind := "an irregular file"
	switch {
	case mode.IsDir():
		kind = "a directory"
	case mode&fs.ModeNamedPipe != 0:
		kind = "a named pipe"
	case mode&fs.ModeSocket != 0:
		kind = "a socket"
	case mode&fs.ModeDevice != 0:
		kind = "a device"
	}
	return fmt.Errorf("--image %s: not a regular file (%s)", path, kind)
}

// sniffedImageType reports the image media type http.DetectContentType
// recognises, without a parameter. Anything else, including image/bmp, is
// refused: the extension is not consulted.
func sniffedImageType(data []byte) (string, bool) {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct, true
	default:
		return ct, false
	}
}
