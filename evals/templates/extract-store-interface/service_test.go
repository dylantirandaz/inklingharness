package fixture

import (
	"errors"
	"testing"
)

var _ Store = (*FileStore)(nil)

var errMissing = errors.New("missing")

type memoryStore map[string][]byte

func (m memoryStore) Load(key string) ([]byte, error) {
	data, found := m[key]
	if !found {
		return nil, errMissing
	}
	return data, nil
}

func (m memoryStore) Save(key string, data []byte) error {
	m[key] = data
	return nil
}

func (m memoryStore) Delete(key string) error {
	delete(m, key)
	return nil
}

func TestRename(t *testing.T) {
	store := memoryStore{"a": []byte("text")}
	if err := NewService(store).Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, found := store["a"]; found || string(store["b"]) != "text" {
		t.Fatalf("store after rename = %v", store)
	}
	if err := NewService(store).Rename("zzz", "c"); !errors.Is(err, errMissing) {
		t.Fatalf("Rename of missing key error = %v", err)
	}
}

func TestRenameWithFiles(t *testing.T) {
	store := &FileStore{Directory: t.TempDir()}
	if err := store.Save("a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := NewService(store).Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load("b"); err != nil || string(data) != "x" {
		t.Fatalf("Load(b) = %q, %v", data, err)
	}
}
