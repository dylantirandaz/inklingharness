package store

import "fixture/hashing"

// Store keeps blobs by content key.
type Store struct {
	blobs map[string][]byte
}

// New returns an empty store.
func New() *Store {
	return &Store{blobs: make(map[string][]byte)}
}

// Put stores data and returns its key.
func (s *Store) Put(data []byte) string {
	key := hashing.LegacyHash(data)
	s.blobs[key] = append([]byte(nil), data...)
	return key
}

// Get returns the data for key.
func (s *Store) Get(key string) ([]byte, bool) {
	data, found := s.blobs[key]
	return data, found
}
