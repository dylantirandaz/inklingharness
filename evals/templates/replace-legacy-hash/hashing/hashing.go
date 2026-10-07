package hashing

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
)

// Hash returns the hex SHA-256 digest of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// LegacyHash returns the hex MD5 digest of data.
//
// Deprecated: use Hash.
func LegacyHash(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
