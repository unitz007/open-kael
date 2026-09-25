package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a random 32-character hex string suitable for use as a
// primary key. Uses crypto/rand — never errors on supported platforms.
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
