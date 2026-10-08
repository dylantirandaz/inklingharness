package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

const (
	MaxImageBytes    = 5 << 20
	contextPageBytes = 16 << 10
	// A 128-bit content reference is shorter to copy than the full digest.
	resourceIDBytes = 16
)

// ImageReference names an immutable image in the private session store.
// The source file can change or disappear without changing this image.
type ImageReference struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size_bytes"`
}

// ContextPage is an exact UTF-8 byte range of an archived conversation.
// NextOffset is nil at the end; otherwise it starts the next complete rune.
type ContextPage struct {
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset *int64 `json:"next_offset,omitempty"`
	TotalBytes int64  `json:"total_bytes"`
}

// CaptureImage reads at most MaxImageBytes+1 bytes and publishes the image only
// after its format and size pass. Equal images share one immutable file.
func (s *Store) CaptureImage(ctx context.Context, mediaType string, input io.Reader) (ImageReference, error) {
	var size int64
	id, err := s.captureResource(ctx, "images", func(output io.Writer) error {
		var prefix [12]byte
		count, err := io.ReadFull(input, prefix[:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		actual, known := imageMediaType(prefix[:count])
		if !known {
			return fmt.Errorf("content is not a supported image")
		}
		if actual != mediaType {
			return fmt.Errorf("expected %s, but content is %s", mediaType, actual)
		}
		if _, err := io.WriteString(output, mediaType+"\n"); err != nil {
			return err
		}
		source := io.MultiReader(bytes.NewReader(prefix[:count]), input)
		size, err = io.Copy(output, contextReader{ctx, io.LimitReader(source, MaxImageBytes+1)})
		if err != nil {
			return err
		}
		if size > MaxImageBytes {
			return fmt.Errorf("image exceeds %d bytes", MaxImageBytes)
		}
		return nil
	})
	if err != nil {
		return ImageReference{}, err
	}
	return ImageReference{ID: id, MediaType: mediaType, Size: size}, nil
}

// CaptureImageBlock imports an inline protocol image without a decoded copy.
func (s *Store) CaptureImageBlock(ctx context.Context, image anthropic.ImageBlock) (ImageReference, error) {
	return s.CaptureImage(ctx, image.MediaType, base64.NewDecoder(base64.StdEncoding, strings.NewReader(image.Data)))
}

// LoadImage opens only an image ID, not an arbitrary filesystem path.
func (s *Store) LoadImage(ctx context.Context, id string) (anthropic.ImageBlock, error) {
	path, err := s.resourcePath("images", id)
	if err != nil {
		return anthropic.ImageBlock{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return anthropic.ImageBlock{}, fmt.Errorf("open image %s: %w", id, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return anthropic.ImageBlock{}, err
	}
	reader := bufio.NewReaderSize(file, 256)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return anthropic.ImageBlock{}, fmt.Errorf("read image header: %w", err)
	}
	mediaType := string(header[:len(header)-1])
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return anthropic.ImageBlock{}, fmt.Errorf("invalid stored image type %q", mediaType)
	}
	size := info.Size() - int64(len(header))
	if !info.Mode().IsRegular() || size <= 0 || size > MaxImageBytes {
		return anthropic.ImageBlock{}, errors.New("invalid stored image size or file type")
	}
	var data strings.Builder
	data.Grow(base64.StdEncoding.EncodedLen(int(size)))
	encoder := base64.NewEncoder(base64.StdEncoding, &data)
	if _, err := io.Copy(encoder, contextReader{ctx, reader}); err != nil {
		return anthropic.ImageBlock{}, err
	}
	if err := encoder.Close(); err != nil {
		return anthropic.ImageBlock{}, err
	}
	return anthropic.ImageBlock{MediaType: mediaType, Data: data.String()}, nil
}

// ArchiveContext keeps exact messages outside active model context. Archives
// are not command outputs and are never removed by output pruning.
func (s *Store) ArchiveContext(ctx context.Context, messages []anthropic.EncodedMessage) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("cannot archive an empty conversation")
	}
	return s.captureResource(ctx, "context", func(output io.Writer) error {
		for _, message := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(message.Wire()) == 0 {
				return errors.New("cannot archive an empty message")
			}
			if _, err := output.Write(message.Wire()); err != nil {
				return err
			}
			if _, err := io.WriteString(output, "\n"); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReadContext returns a bounded page. offset must be a rune boundary, such as
// zero or the NextOffset from the previous page.
func (s *Store) ReadContext(ctx context.Context, id string, offset int64) (ContextPage, error) {
	if offset < 0 {
		return ContextPage{}, errors.New("context offset must be nonnegative")
	}
	if err := ctx.Err(); err != nil {
		return ContextPage{}, err
	}
	path, err := s.resourcePath("context", id)
	if err != nil {
		return ContextPage{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ContextPage{}, fmt.Errorf("open context %s: %w", id, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ContextPage{}, err
	}
	if !info.Mode().IsRegular() || offset > info.Size() {
		return ContextPage{}, errors.New("context offset exceeds the archive or the archive is not a regular file")
	}
	buffer := make([]byte, min(int64(contextPageBytes), info.Size()-offset))
	count, err := file.ReadAt(buffer, offset)
	if err != nil && err != io.EOF {
		return ContextPage{}, err
	}
	buffer = buffer[:count]
	if len(buffer) > 0 && !utf8.RuneStart(buffer[0]) {
		return ContextPage{}, errors.New("context offset is inside a UTF-8 rune; use next_offset from the previous page")
	}
	if offset+int64(len(buffer)) < info.Size() {
		for trimmed := 0; trimmed < utf8.UTFMax-1 && len(buffer) > 0 && !utf8.Valid(buffer); trimmed++ {
			buffer = buffer[:len(buffer)-1]
		}
	}
	if !utf8.Valid(buffer) {
		return ContextPage{}, errors.New("context archive contains invalid UTF-8")
	}
	if err := ctx.Err(); err != nil {
		return ContextPage{}, err
	}
	page := ContextPage{Text: string(buffer), Offset: offset, TotalBytes: info.Size()}
	if next := offset + int64(len(buffer)); next < info.Size() {
		page.NextOffset = &next
	}
	return page, nil
}

func (s *Store) resourcePath(kind, id string) (string, error) {
	if s.root == "" {
		return "", errors.New("session: empty store directory")
	}
	if len(id) != resourceIDBytes*2 {
		return "", errors.New("resource ID must have 32 lowercase hexadecimal characters")
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return "", errors.New("resource ID must have 32 lowercase hexadecimal characters")
		}
	}
	return filepath.Join(s.root, "resources", kind, id), nil
}

// captureResource hashes while writing. A hard link publishes the complete
// object without replacing an equal object that another process stored.
func (s *Store) captureResource(ctx context.Context, kind string, write func(io.Writer) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.prepareRoot(); err != nil {
		return "", err
	}
	directory := filepath.Join(s.root, "resources", kind)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, ".capture-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	digest := sha256.New()
	if err := write(io.MultiWriter(file, digest)); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	id := hex.EncodeToString(digest.Sum(nil)[:resourceIDBytes])
	if err := os.Link(file.Name(), filepath.Join(directory, id)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return id, nil
}

type contextReader struct {
	context context.Context
	reader  io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.context.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func imageMediaType(content []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(content, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", true
	case bytes.HasPrefix(content, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", true
	case bytes.HasPrefix(content, []byte("GIF87a")), bytes.HasPrefix(content, []byte("GIF89a")):
		return "image/gif", true
	case len(content) >= 12 && bytes.Equal(content[:4], []byte("RIFF")) && bytes.Equal(content[8:12], []byte("WEBP")):
		return "image/webp", true
	default:
		return "", false
	}
}
