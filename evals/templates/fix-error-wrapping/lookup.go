package fixture

import "fmt"

// Lookup returns the value for key and adds the key to any error.
func Lookup(s *Store, key string) (string, error) {
	value, err := s.Get(key)
	if err != nil {
		return "", fmt.Errorf("lookup %q: %v", key, err)
	}
	return value, nil
}
