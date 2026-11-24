// Codex responses_lite catalogs must reach the same terminal contract as
// top-level declarations. These fixtures contain no live prompts or accounts.
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesLiteCatalogMergesNamespaceChildren(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"function","name":"wait","parameters":{"type":"object"}}]}]}]}`)
	chat, _, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := chatToolsByName(t, chat)
	if len(tools) != 2 || tools["exec"] == nil || tools["wait"] == nil {
		t.Fatalf("merging a namespace discarded a newly declared tool: %v", tools)
	}
}

func TestResponsesLiteCatalogRejectsMalformedDeclarations(t *testing.T) {
	for _, catalog := range []string{
		`{}`, `{"tools":null}`, `{"tools":"not-an-array"}`, `{"tools":[null]}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":null}]}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":[false]}]}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"future","name":"lookup"}]}]}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"web_search","name":"lookup"}]}]}`,
		`{"tools":[{"type":"function","name":"lookup","strict":"yes"}]}`,
		`{"tools":[{"type":"function","name":""}]}`,
	} {
		t.Run(catalog, func(t *testing.T) {
			var item map[string]any
			_ = json.Unmarshal([]byte(catalog), &item)
			item["type"] = "additional_tools"
			item["role"] = "developer"
			body, _ := json.Marshal(map[string]any{"model": "m", "input": []any{item}})
			if _, _, err := responsesToChat(body); err == nil {
				t.Fatal("malformed lite catalog was silently discarded")
			}
		})
	}
}

func TestResponsesLiteCatalogRejectsConflictingIdentities(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"custom","name":"exec"}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}]}]}`)
	if _, _, err := responsesToChat(body); err == nil {
		t.Fatal("one public identity with conflicting tool kinds was accepted")
	}
}

func TestResponsesLiteNamespaceIdentityIsCaseSensitive(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"Functions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}]}`)
	chat, req, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := chatToolsByName(t, chat)
	if len(tools) != 1 || tools["Functions__lookup"] == nil {
		t.Fatalf("an explicit namespace was collapsed into the default: %v", tools)
	}
	if got := upstreamToolName(req.toolAliasIndex(), "Functions", "lookup"); got != "Functions__lookup" {
		t.Fatalf("history alias disagrees with declaration: %s", got)
	}
}

func TestResponsesDefaultNamespaceDoesNotMutateEcho(t *testing.T) {
	body := []byte(`{"model":"m","input":"next","tools":[{"type":"function","namespace":"functions","name":"lookup","parameters":{"type":"object"}}]}`)
	_, req, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	echo := map[string]any{}
	req.applyEcho(echo)
	tool := echo["tools"].([]any)[0].(map[string]any)
	if tool["namespace"] != "functions" {
		t.Fatal("translation changed the client's original tool declaration")
	}
}

func TestResponsesLiteTerminalContractAndCustomRoundTrip(t *testing.T) {
	catalog := `[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"function","name":"wait","parameters":{"type":"object"}}]}]`
	for _, test := range []struct {
		name, choice string
		valid        bool
	}{
		{"auto", `"auto"`, true},
		{"required", `"required"`, true},
		{"forced_custom", `{"type":"custom","name":"exec"}`, true},
		{"allowed_custom", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"custom","name":"exec"}]}`, true},
		{"none", `"none"`, false},
		{"excluded_exec", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"wait"}]}`, false},
	} {
		for _, stream := range []bool{false, true} {
			suffix := map[bool]string{false: "/json", true: "/stream"}[stream]
			t.Run(test.name+suffix, func(t *testing.T) {
				request := `{"model":"m","stream":true,"tool_choice":` + test.choice + `,"input":[{"type":"additional_tools","role":"developer","tools":` + catalog + `},{"type":"message","role":"user","content":"next"}]}`
				response, names, values, err := protocolDiagnosisReply(t, request, protocolDiagnosisMessage([]string{"exec"}, `{"input":"text('ok')"}`), "tool_calls", stream)
				if !test.valid {
					if (stream && response["status"] != "failed") || (!stream && err == nil) {
						t.Fatal("lite catalog widened the tool_choice boundary")
					}
					for i, name := range names {
						if strings.HasSuffix(name, "_input.done") || strings.HasSuffix(name, "_arguments.done") {
							t.Fatal("a forbidden tool was marked executable")
						}
						if name == "response.output_item.done" {
							item, _ := values[i]["item"].(map[string]any)
							if item["type"] == "custom_tool_call" || item["type"] == "function_call" {
								t.Fatal("a forbidden call item was completed")
							}
						}
					}
					return
				}
				if err != nil || response["status"] != "completed" {
					t.Fatalf("original lite tool failure persists: status=%v err=%v", response["status"], err)
				}
				for _, raw := range response["output"].([]any) {
					item := raw.(map[string]any)
					if item["type"] == "custom_tool_call" {
						if item["name"] != "exec" || item["input"] != "text('ok')" || item["call_id"] != "call_exec" || item["namespace"] != nil {
							t.Fatalf("custom identity/input/pairing drifted: %v", item)
						}
						return
					}
				}
				t.Fatal("completed response lost the custom call")
			})
		}
	}
}
