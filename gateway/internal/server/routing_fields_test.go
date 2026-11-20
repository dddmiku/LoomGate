package server

import (
	"strings"
	"testing"
)

func TestChatRoutingFieldsRetainCaseInsensitiveSDKCompatibility(t *testing.T) {
	h, _, key, _ := providerFixture(t, nil)
	for _, body := range []string{
		`{"model":"fixture:dynamic","messages":[{"role":"user","content":"fixture"}],"Stream":true}`,
		`{"model":"fixture:dynamic","messages":[{"role":"user","content":"fixture"}],"STREAM":true}`,
	} {
		response := providerRequest(h, key, body)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "[DONE]") {
			t.Fatal("legacy case-insensitive stream routing changed")
		}
	}
	for _, name := range []string{"n", "N"} {
		response := providerRequest(h, key, `{"model":"fixture:dynamic","messages":[{"role":"user","content":"fixture"}],"`+name+`":2}`)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "single choice") {
			t.Fatal("choice-count routing compatibility changed")
		}
	}
}
