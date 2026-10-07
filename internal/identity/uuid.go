package identity

import (
	"crypto/sha1"
	"encoding/hex"
)

// namespace is JellyBloom's UUIDv5 namespace (a fixed random UUID).
var namespace = [16]byte{0x6a, 0x3b, 0x1f, 0x52, 0x9c, 0x4e, 0x4d, 0x0b, 0xa8, 0x77, 0x2e, 0x5d, 0xc1, 0x90, 0x3f, 0x64}

// newID returns the UUIDv5 of name in JellyBloom's namespace, formatted like
// Jellyfin item IDs: 32 lowercase hex digits, no dashes.
func newID(name string) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return hex.EncodeToString(u[:])
}
