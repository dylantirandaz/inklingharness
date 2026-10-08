package attachment_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/session"
)

const maxImageBytes = 5 * 1024 * 1024

func smallImage() image.Image {
	picture := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	picture.SetColorIndex(1, 1, 1)
	return picture
}

func encodedPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, smallImage()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodedJPEG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, smallImage(), nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodedGIF(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := gif.Encode(&buffer, smallImage(), nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// encodedWEBP builds a 1x1 lossless WebP file. The standard library has no
// WebP encoder, so the test writes the RIFF container and VP8L chunk directly.
func encodedWEBP() []byte {
	chunk := []byte{0x2f, 0x00, 0x00, 0x00, 0x00, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00}
	var file bytes.Buffer
	file.WriteString("RIFF")
	_ = binary.Write(&file, binary.LittleEndian, uint32(4+8+len(chunk)))
	file.WriteString("WEBPVP8L")
	_ = binary.Write(&file, binary.LittleEndian, uint32(len(chunk)))
	file.Write(chunk)
	return file.Bytes()
}

func writeBytes(t *testing.T, root, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type imageEnvelope struct {
	Request     string `json:"request"`
	Attachments []struct {
		Path      string  `json:"path"`
		Size      int64   `json:"size_bytes"`
		MediaType string  `json:"media_type"`
		ImageID   string  `json:"image_id"`
		Content   *string `json:"content"`
	} `json:"attachments"`
}

func decodeImageEnvelope(t *testing.T, prompt string) imageEnvelope {
	t.Helper()
	_, payload, found := strings.Cut(prompt, "\n")
	if !found {
		t.Fatal("attachment metadata is missing")
	}
	var envelope imageEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestPreparedImageReferencesKeepExactSnapshots(t *testing.T) {
	root := t.TempDir()
	store := session.NewStore(t.TempDir())
	contents := map[string][]byte{
		"a.png": encodedPNG(t), "c.gif": encodedGIF(t), "b.jpg": encodedJPEG(t),
		"d.WEBP": encodedWEBP(), "e.jpeg": encodedJPEG(t),
	}
	for name, content := range contents {
		writeBytes(t, root, name, content)
	}
	writeText(t, root, "note.txt", "text stays inline")
	prepared, err := attachment.Prepare(context.Background(), root,
		"Describe @b.jpg @note.txt @d.WEBP @a.png @e.jpeg", []string{"a.png", "c.gif"}, store)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"a.png", "c.gif", "b.jpg", "note.txt", "d.WEBP", "e.jpeg"}
	var paths []string
	for _, file := range prepared.Files {
		paths = append(paths, filepath.Base(file.Path))
	}
	if !reflect.DeepEqual(paths, order) {
		t.Fatalf("attachment order = %v", paths)
	}
	envelope := decodeImageEnvelope(t, prepared.Prompt)
	if len(envelope.Attachments) != len(order) {
		t.Fatal("attachment references were lost")
	}
	for index, name := range order {
		entry := envelope.Attachments[index]
		if name == "note.txt" {
			if entry.Content == nil || *entry.Content != "text stays inline" {
				t.Fatal("text attachment changed")
			}
			continue
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		image, err := store.LoadImage(context.Background(), entry.ImageID)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil || !bytes.Equal(decoded, contents[name]) || image.MediaType != entry.MediaType {
			t.Fatalf("stored %s changed after source removal: %v", name, err)
		}
		if strings.Contains(prepared.Prompt, image.Data) || entry.Content != nil {
			t.Fatal("image bytes entered the text conversation")
		}
	}
	if envelope.Attachments[2].ImageID != envelope.Attachments[5].ImageID {
		t.Fatal("equal image content did not share a stored image")
	}
}

func TestPrepareRejectsBadImagesWithoutPartialMessage(t *testing.T) {
	root := t.TempDir()
	store := session.NewStore(t.TempDir())
	writeBytes(t, root, "valid.png", encodedPNG(t))
	writeBytes(t, root, "jpeg.png", encodedJPEG(t))
	writeBytes(t, root, "png.webp", encodedPNG(t))
	writeBytes(t, root, "riff.webp", []byte("RIFF\x04\x00\x00\x00WAVE"))
	writeBytes(t, root, "text.gif", []byte("GIF8 is not a header"))
	writeBytes(t, root, "short.jpg", []byte{0xFF, 0xD8})
	writeBytes(t, root, "empty.png", nil)
	for _, name := range []string{"jpeg.png", "png.webp", "riff.webp", "text.gif", "short.jpg", "empty.png", "missing.png", root} {
		t.Run(filepath.Base(name), func(t *testing.T) {
			prepared, err := attachment.Prepare(context.Background(), root, "Review", []string{"valid.png", name}, store)
			if err == nil || prepared.Prompt != "" || prepared.Files != nil {
				t.Fatalf("invalid image returned a partial message: %+v, %v", prepared, err)
			}
		})
	}
}

func TestPrepareImageSizeLimit(t *testing.T) {
	root := t.TempDir()
	store := session.NewStore(t.TempDir())
	content := make([]byte, maxImageBytes)
	copy(content, encodedPNG(t))
	writeBytes(t, root, "limit.png", content)
	prepared, err := attachment.Prepare(context.Background(), root, "Review @limit.png", nil, store)
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeImageEnvelope(t, prepared.Prompt)
	image, err := store.LoadImage(context.Background(), envelope.Attachments[0].ImageID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil || !bytes.Equal(decoded, content) {
		t.Fatal("the exact-limit image was not retained whole")
	}
	writeText(t, root, "large.txt", strings.Repeat("a", 256*1024))
	if _, err := attachment.Prepare(context.Background(), root, "Review @limit.png @large.txt", nil, store); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, root, "over.png", append(content, 0))
	failed, err := attachment.Prepare(context.Background(), root, "Review @over.png", nil, store)
	if err == nil || failed.Prompt != "" || failed.Files != nil {
		t.Fatalf("oversize image returned a message: %+v, %v", failed, err)
	}
}

func TestPrepareManyImagesUsesReferences(t *testing.T) {
	root := t.TempDir()
	store := session.NewStore(t.TempDir())
	content := encodedGIF(t)
	var names []string
	for index := range 24 {
		name := fmt.Sprintf("image%d.gif", index)
		writeBytes(t, root, name, content)
		names = append(names, name)
	}
	prepared, err := attachment.Prepare(context.Background(), root, "Review @image0.gif", names, store)
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeImageEnvelope(t, prepared.Prompt)
	if len(envelope.Attachments) != len(names) || len(prepared.Files) != len(names) {
		t.Fatal("distinct paths were dropped, or the repeated path was not removed")
	}
	first := envelope.Attachments[0].ImageID
	for index, entry := range envelope.Attachments {
		if entry.ImageID != first || entry.Path != filepath.Join(root, names[index]) {
			t.Fatal("image references lost source identity or content deduplication")
		}
	}
	if strings.Contains(prepared.Prompt, base64.StdEncoding.EncodeToString(content)) {
		t.Fatal("bulk images were inlined")
	}
}
