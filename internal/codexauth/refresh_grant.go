package codexauth

import (
	"crypto/sha256"
	"fmt"
)

// RefreshGrantKey identifies a one-time OAuth grant without storing its plaintext.
func RefreshGrantKey(clientID, refreshToken string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(clientID+"\x00"+refreshToken)))
}
