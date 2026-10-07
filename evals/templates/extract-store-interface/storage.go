package fixture

import (
	"os"
	"path/filepath"
)

// FileStore keeps one file per key in a directory.
type FileStore struct {
	Directory string
}

// Load returns the data for key.
func (s *FileStore) Load(key string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Directory, key))
}

// Save writes the data for key.
func (s *FileStore) Save(key string, data []byte) error {
	return os.WriteFile(filepath.Join(s.Directory, key), data, 0o600)
}

// Delete removes key.
func (s *FileStore) Delete(key string) error {
	return os.Remove(filepath.Join(s.Directory, key))
}
