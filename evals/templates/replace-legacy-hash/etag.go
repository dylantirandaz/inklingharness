package fixture

import "fixture/hashing"

// ETag returns the quoted entity tag of a response body.
func ETag(body []byte) string {
	return `"` + hashing.LegacyHash(body)[:16] + `"`
}
