package providercatalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

const pipelineCatalog = `{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":true,"mode_list":{"item_list":[{"mode_id":"synthetic-mode","name":"Synthetic","model_list":{"item_list":[{"model_item_key":"synthetic-text","name":"Synthetic text","show_task_tools":0},{"model_item_key":"synthetic-work","name":"Synthetic work","show_task_tools":1}]}}]}}}}]}`

type pipelineSession struct{ runtime bool }

func (s pipelineSession) Session(context.Context, string) (doubao.Session, error) {
	v := doubao.Session{BotID: "synthetic-bot", Revision: "1"}
	if s.runtime {
		v.Runtime = &doubao.RuntimeBinding{Identity: strings.Repeat("a", 64)}
	}
	return v, nil
}

type pipelineRuntime struct {
	calls int
	fail  bool
}

func (r *pipelineRuntime) Catalog(context.Context, doubao.RuntimeBinding, string) (json.RawMessage, error) {
	r.calls++
	if r.fail {
		return nil, errors.New("synthetic transport error")
	}
	return json.RawMessage(pipelineCatalog), nil
}

type pipelineHTTP func(*http.Request) (*http.Response, error)

func (f pipelineHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRealCatalogClientToManagementHTTPPreservesRuntimeCapabilities(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "runtime"}[bound], func(t *testing.T) {
			runtime := &pipelineRuntime{}
			legacyCalls := 0
			var client *doubao.Client
			if bound {
				client = doubao.NewWithRuntime(pipelineSession{true}, runtime)
			} else {
				client = doubao.New(pipelineSession{}, pipelineHTTP(func(*http.Request) (*http.Response, error) {
					legacyCalls++
					if runtime.fail {
						return nil, errors.New("synthetic transport error")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":` + pipelineCatalog + `}`))}, nil
				}))
			}
			service := New(Options{Accounts: accountList{rows: []accountvault.Account{acct("synthetic-account", true)}}, Doubao: client, DoubaoInference: true})
			for _, phase := range []string{"fresh", "cached"} {
				response := request(t, service, "/workbench/models?provider=doubao&account=synthetic-account")
				page := decodePage(t, response)
				row := page.Catalogs[0]
				want := "catalog_only"
				if bound {
					want = "single_turn_text"
				}
				if row.Status != "ready" || row.RoutingStatus != want || len(row.Models) != 2 {
					t.Fatalf("%s wrong catalog route", phase)
				}
				if (row.StreamingMode == "incremental") != bound {
					t.Fatal("stream capability differs from runtime availability")
				}
				for _, model := range row.Models {
					if model.InferenceAvailable != bound {
						t.Fatalf("%s wrong inference capability", phase)
					}
				}
				if strings.Contains(strings.ToLower(response.Body.String()), "runtimebound") || strings.Contains(response.Body.String(), "runtime_bound") {
					t.Fatal("private metadata exposed")
				}
			}
			if runtime.calls+legacyCalls != 1 {
				t.Fatal("cache path was not exercised")
			}
			runtime.fail = true
			for _, phase := range []string{"stale", "backoff"} {
				page := decodePage(t, request(t, service, "/workbench/models?provider=doubao&account=synthetic-account&refresh=1"))
				row := page.Catalogs[0]
				if row.Status != "stale" || len(row.Models) != 2 {
					t.Fatalf("%s lost stale models", phase)
				}
				if row.StreamingMode != "" {
					t.Fatal("stale directory advertised live streaming")
				}
				for _, model := range row.Models {
					if model.InferenceAvailable {
						t.Fatal("stale catalog advertised callable model")
					}
				}
			}
			if runtime.calls+legacyCalls != 2 {
				t.Fatal("backoff retried upstream")
			}
		})
	}
}
