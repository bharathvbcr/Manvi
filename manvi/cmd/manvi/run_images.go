package main

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

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
// before the --timeout context exists, so nothing here may wait on the path: it
// has to be a regular file (a symbolic link is followed, as it always was, and
// what it leads to decides), and it is checked twice. Once by name, so a FIFO is
// refused without being opened; and once on the opened handle, which is the
// check that holds if the name was swapped for something else in between. The
// open itself does not wait for a writer, for the same swap.
func loadRunImage(path string) (block llm.ImageBlock, err error) {
	if named, statErr := os.Stat(path); statErr == nil && !named.Mode().IsRegular() {
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
	if !info.Mode().IsRegular() {
		return llm.ImageBlock{}, notRegularImage(path, info.Mode())
	}
	if info.Size() > maxImageBytes {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: %d bytes exceeds the 8 MiB limit", path, info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if err != nil {
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
