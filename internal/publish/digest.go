package publish

import "crypto/sha256"

// sha256Bytes returns the SHA-256 digest of data.
func sha256Bytes(data []byte) [32]byte {
	return sha256.Sum256(data)
}
