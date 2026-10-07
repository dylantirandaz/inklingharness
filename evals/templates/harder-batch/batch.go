package inventory

import "fmt"

// Apply processes changes in order as one transaction.
func (s *Store) Apply(changes []Change) error {
	for i, change := range changes {
		if change.Key == "" { return fmt.Errorf("change %d: %w", i, ErrInvalid) }
		switch change.Kind {
		case "set":
			if change.Item.Count < 0 { return fmt.Errorf("change %d: %w", i, ErrInvalid) }
			s.items[change.Key] = change.Item
		case "add":
			item, ok := s.items[change.Key]
			if !ok { return fmt.Errorf("change %d: %w", i, ErrMissing) }
			item.Count += change.Delta
			if item.Count < 0 { return fmt.Errorf("change %d: %w", i, ErrInvalid) }
			s.items[change.Key] = item
		case "delete":
			if _, ok := s.items[change.Key]; !ok { return fmt.Errorf("change %d: %w", i, ErrMissing) }
			delete(s.items, change.Key)
		default:
			return fmt.Errorf("change %d: %w", i, ErrInvalid)
		}
	}
	return nil
}
