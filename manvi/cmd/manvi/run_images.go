package main

import (
	"fmt"
	"io"
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

func loadRunImage(path string) (block llm.ImageBlock, err error) {
	f, err := os.Open(path) // #nosec G304 -- the operator named this --image path; it is read and sniffed, never executed
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
	if info.IsDir() {
		return llm.ImageBlock{}, fmt.Errorf("--image %s: is a directory", path)
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
