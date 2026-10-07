package cache

import (
	"strconv"

	"fixture/hashing"
)

// Key returns the cache key of one request.
func Key(method, path string, version int) string {
	return hashing.LegacyHash([]byte(method + " " + path + " " + strconv.Itoa(version)))
}
