package salt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"net/http"
)

func saltFromAPIKey(apiKey string) string {
	hash := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(hash[:4]) // Use first 4 bytes (32 bits)
}

func randomSalt() string {
	randomBytes := make([]byte, 4)
	if _, err := rand.Read(randomBytes); err != nil {
		return ""
	}
	return hex.EncodeToString(randomBytes)
}

func SaltFromRequest(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		apiKey := strings.TrimPrefix(authHeader, "Bearer ")
		if apiKey != "" {
			return saltFromAPIKey(apiKey)
		}
	}
	return randomSalt()
}

// InjectSalt adds salt to the request body
func InjectSalt(body map[string]interface{}, salt string) {
	body["cache_salt"] = salt
}
