package fixture

import "errors"

// ParseSize parses a size such as "512", "10KiB", or "1.5MiB" into bytes.
func ParseSize(text string) (int64, error) {
	return 0, errors.New("ParseSize: not implemented")
}
