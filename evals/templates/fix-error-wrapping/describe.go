package fixture

// Describe returns a line for people to read.
func Describe(s *Store, key string) string {
	value, err := Lookup(s, key)
	if err == ErrNotFound {
		return "missing: " + key
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return key + " = " + value
}
