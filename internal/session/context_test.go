package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
)

func TestContextArchiveReadsExactUTF8Pages(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	messages := []anthropic.EncodedMessage{text(t, strings.Repeat("é界x", 9000)), text(t, "final detail\nwith a second line")}
	id, err := store.ArchiveContext(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	for _, message := range messages {
		expected.Write(message.Wire())
		expected.WriteByte('\n')
	}
	store = NewStore(root)
	var actual strings.Builder
	var offset int64
	for {
		page, err := store.ReadContext(context.Background(), id, offset)
		if err != nil {
			t.Fatal(err)
		}
		if page.Offset != offset || page.TotalBytes != int64(expected.Len()) || len(page.Text) > contextPageBytes || !utf8.ValidString(page.Text) {
			t.Fatalf("invalid archive page at %d", offset)
		}
		actual.WriteString(page.Text)
		if page.NextOffset == nil {
			break
		}
		if *page.NextOffset != offset+int64(len(page.Text)) || *page.NextOffset <= offset {
			t.Fatal("archive pages overlap, lose bytes, or fail to advance")
		}
		offset = *page.NextOffset
	}
	if actual.String() != expected.String() {
		t.Fatal("archive pages did not preserve the exact messages")
	}
	insideRune := int64(bytes.Index(expected.Bytes(), []byte("界")) + 1)
	for _, invalid := range []int64{-1, insideRune, int64(expected.Len() + 1)} {
		if _, err := store.ReadContext(context.Background(), id, invalid); err == nil {
			t.Fatalf("accepted invalid offset %d", invalid)
		}
	}
	end, err := store.ReadContext(context.Background(), id, int64(expected.Len()))
	if err != nil || end.Text != "" || end.NextOffset != nil {
		t.Fatalf("invalid end of archive: %+v, %v", end, err)
	}
	before, err := os.Stat(filepath.Join(root, "resources", "context", id))
	if err != nil {
		t.Fatal(err)
	}
	sameID, err := store.ArchiveContext(context.Background(), messages)
	if err != nil || sameID != id {
		t.Fatalf("equal archives did not share an ID: %v", err)
	}
	after, err := os.Stat(filepath.Join(root, "resources", "context", id))
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatalf("archive was replaced or was not private: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadContext(ctx, id, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("archive read ignored cancellation: %v", err)
	}
}

func TestContextReadRejectsDamagedUTF8(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	id, err := store.ArchiveContext(context.Background(), []anthropic.EncodedMessage{text(t, strings.Repeat("source ", 5000))})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "resources", "context", id)
	damaged := append([]byte("damaged: \xff"), bytes.Repeat([]byte("x"), 2*contextPageBytes)...)
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadContext(context.Background(), id, 0); err == nil {
		t.Fatal("damaged archive produced a partial or empty successful page")
	}
}

func TestResourceIDsCannotReadOtherPaths(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, id := range []string{"../private", strings.Repeat("a", 31) + "/", strings.Repeat("A", 32), strings.Repeat("0", 33)} {
		if _, err := store.ReadContext(context.Background(), id, 0); err == nil {
			t.Fatalf("context read accepted path %q", id)
		}
		if _, err := store.LoadImage(context.Background(), id); err == nil {
			t.Fatalf("image read accepted path %q", id)
		}
	}
}

func TestInvalidInlineImageDoesNotPublish(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	_, err := store.CaptureImageBlock(context.Background(), anthropic.ImageBlock{MediaType: "image/png", Data: "iVBORw0KGgoAAAAA!"})
	if err == nil {
		t.Fatal("invalid base64 image was accepted")
	}
	entries, err := os.ReadDir(filepath.Join(root, "resources", "images"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid image left a partial resource: %v, %v", entries, err)
	}
}
