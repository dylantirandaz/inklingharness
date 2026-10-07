package fixture

import "fmt"

// Service moves documents between keys.
type Service struct {
	store *FileStore
}

// NewService returns a service that uses store.
func NewService(store *FileStore) *Service {
	return &Service{store: store}
}

// Rename moves the document at from to to.
func (s *Service) Rename(from, to string) error {
	data, err := s.store.Load(from)
	if err != nil {
		return fmt.Errorf("rename %s: %w", from, err)
	}
	if err := s.store.Save(to, data); err != nil {
		return fmt.Errorf("rename %s: %w", from, err)
	}
	return s.store.Delete(from)
}
