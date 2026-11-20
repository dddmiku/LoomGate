package trae

import (
	"encoding/json"
	"net/http"
)

// ChatAuthRejected is the chat service's explicit revoked-token response.
// A normal parameter error, quoted code in prose or a successful stream never
// authorizes a credential refresh or a generation retry.
func ChatAuthRejected(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	var envelope struct {
		Code *int `json:"code"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Code != nil && *envelope.Code == AuthFailedCode
}
