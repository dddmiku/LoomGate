package server

import (
	"encoding/json"
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

func TestChatRoutingFieldsRejectAmbiguousCaseInsensitiveAliases(t *testing.T) {
	for _, field := range []string{"model", "stream", "n"} {
		t.Run(field, func(t *testing.T) {
			_, err := chatRoutingField(map[string]json.RawMessage{
				field:                  json.RawMessage(`1`),
				strings.ToUpper(field): json.RawMessage(`2`),
			}, field)
			if err == nil {
				t.Fatal("ambiguous routing aliases were accepted")
			}
		})
	}
	for _, field := range []string{"Model", "STREAM", "N"} {
		raw, err := chatRoutingField(map[string]json.RawMessage{field: json.RawMessage(`1`)}, strings.ToLower(field))
		if err != nil || string(raw) != `1` {
			t.Fatalf("single case-insensitive alias changed: raw=%s err=%v", raw, err)
		}
	}
}
