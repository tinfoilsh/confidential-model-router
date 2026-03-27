package manager

import (
	"net/http"
	"strings"
)

func extractAuthBillingFields(req *http.Request) (userID, apiKey string) {
	if req == nil {
		return "", ""
	}

	authHeader := req.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", ""
	}

	apiKey = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if apiKey == "" {
		return "", ""
	}

	return "authenticated_user", apiKey
}
