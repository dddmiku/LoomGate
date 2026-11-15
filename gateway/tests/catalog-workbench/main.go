// catalog-workbench serves the real panel and catalog service with synthetic
// upstream transport. It cannot read real accounts or contact an upstream.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/providercatalog"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/taskengine"
	"workbuddy2api/internal/usage"
)

type fixtures struct {
	mode      atomic.Int32
	calls     atomic.Int32
	mutations atomic.Int32
}

func (f *fixtures) List() ([]accountvault.Account, error) {
	if f.mode.Load() == 3 {
		return nil, fmt.Errorf("synthetic_storage_unavailable")
	}
	rows := []accountvault.Account{{ID: "a-fixture", Provider: "doubao", Enabled: true}, {ID: "b-fixture", Provider: "doubao", Enabled: true}, {ID: "c-disabled", Provider: "doubao", Enabled: false}}
	for i := 0; i < 21; i++ {
		rows = append(rows, accountvault.Account{ID: fmt.Sprintf("z-page-%02d", i), Provider: "doubao", Enabled: true})
	}
	return rows, nil
}
func (f *fixtures) Session(ctx context.Context, id string) (doubao.Session, error) {
	if id == "b-fixture" {
		return doubao.Session{}, &doubao.APIError{Kind: "login_required"}
	}
	return doubao.Session{Revision: "synthetic", Headers: http.Header{"X-Fixture-Account": {id}}, BotID: "synthetic"}, nil
}
func (f *fixtures) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "www.doubao.com" || r.URL.Path != "/alice/slot/action_bar_v3/brief_list" {
		return nil, fmt.Errorf("fixture_unexpected_endpoint")
	}
	f.calls.Add(1)
	if f.mode.Load() == 2 {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	code := 200
	model := map[string]any{"item_id": 7, "model_item_key": "fixture-model", "name": "合成模型 <img src=x onerror=alert(1)>", "show_task_tools": 1, "model_extra_params": map[string]any{"total_window_size": "131072"}, "reasoning_effort_config": map[string]any{"default_level": 2, "option_list": []any{map[string]any{"level": 2, "name": "合成档位"}}}, "tag_list": []any{map[string]any{"text": "合成倍率"}}}
	mode := map[string]any{"mode_id": "work", "name": "合成工作模式", "model_list": map[string]any{"item_list": []any{model}}}
	menu := map[string]any{"use_mode_model_list": true, "mode_list": map[string]any{"item_list": []any{mode}}}
	body := map[string]any{"code": 0, "data": map[string]any{"entry_list": []any{map[string]any{"active_switch_conf": map[string]any{"menu_conf_v2": menu}}}}}
	if f.mode.Load() == 1 {
		code = 503
		body = map[string]any{"error": "synthetic_upstream_failure"}
	}
	b, _ := json.Marshal(body)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}
func main() {
	root := flag.String("root", "", "dedicated synthetic runtime directory")
	address := flag.String("listen", "127.0.0.1:17863", "loopback address")
	flag.Parse()
	host, _, err := net.SplitHostPort(*address)
	if err != nil || host != "127.0.0.1" || *root == "" {
		panic("fixture_requires_loopback_and_root")
	}
	for _, dir := range []string{"panel-data", "auths", "data"} {
		if err = os.MkdirAll(filepath.Join(*root, dir), 0700); err != nil {
			panic("fixture_directory_failed")
		}
	}
	config := filepath.Join(*root, "config.json")
	if err = os.WriteFile(config, []byte(`{"api_keys_file":"./data/api_keys.json"}`), 0600); err != nil {
		panic("fixture_config_failed")
	}
	fixture := &fixtures{}
	client := doubao.New(fixture, fixture)
	catalog := providercatalog.New(providercatalog.Options{Accounts: fixture, Doubao: client, Timeout: 3 * time.Second})
	engine, err := taskengine.Open(taskengine.Options{Path: filepath.Join(*root, "data", "synthetic-tasks.json"), PollMin: 25 * time.Millisecond, PollMax: 50 * time.Millisecond, Timeout: 30 * time.Second})
	if err != nil {
		panic("fixture_engine_failed")
	}
	defer engine.Close()
	for _, id := range []string{"workbuddy", "trae", "doubao"} {
		if err = engine.Register(id, &syntheticTasks{provider: id, fixture: fixture}); err != nil {
			panic("fixture_adapter_failed")
		}
	}
	engineContext, engineCancel := context.WithCancel(context.Background())
	defer engineCancel()
	go func() { _ = engine.Run(engineContext) }()
	p := pool.New("")
	defer p.Close()
	ledger, err := usage.Open(filepath.Join(*root, "data", "synthetic-usage.json"), time.Second)
	if err != nil {
		panic("fixture_usage_failed")
	}
	defer ledger.Close()
	journal, err := requestlog.Open(filepath.Join(*root, "data", "synthetic-requests.jsonl"), requestlog.Options{})
	if err != nil {
		panic("fixture_requests_failed")
	}
	defer journal.Close()
	if ledger.Snapshot().Totals.Requests == 0 {
		ledger.Record("synthetic-key-id", "Synthetic client", "fixture-redacted", "cn:fixture-chat", 100, 20, 0, 1, true, time.Now())
	}
	for i := 0; i < 31; i++ {
		start := time.Now().Add(-time.Duration(i+1) * time.Minute)
		err = journal.Append(requestlog.Record{RequestID: fmt.Sprintf("synthetic-request-%02d", i), Protocol: requestlog.ProtocolChat, Model: "cn:fixture-chat", KeyID: "synthetic-key-id", KeyName: "Synthetic client", StartedAt: start, FinishedAt: start.Add(time.Second), Status: requestlog.StatusRejected, HTTPStatus: 429, DurationMS: 1000})
		if err != nil && err != requestlog.ErrDuplicate {
			panic("fixture_seed_failed")
		}
	}
	gateway := server.NewHandler(server.Config{Pool: p, Usage: ledger, Requests: journal})
	management := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if catalog.Route(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/workbench") {
			engine.Handler().ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "cn:fixture-chat", "name": "合成 WorkBuddy 模型", "supports_tool_call": true}}})
		case "/models/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			gateway.InternalHandler().ServeHTTP(w, r)
		}
	})
	admin, err := panel.New(panel.Options{Console: true, BaseDir: *root, AdminDir: filepath.Join(*root, "panel-data"), AuthDir: filepath.Join(*root, "auths"), TrashDir: filepath.Join(*root, "auths-trash"), ConfigPath: config, Runtime: "native", AdminHandler: management, Version: "catalog-fixture"})
	if err != nil {
		panic("fixture_panel_failed")
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/", http.StripPrefix("/admin", admin))
	mux.HandleFunc("GET /__fixture/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"calls": fixture.calls.Load(), "mode": fixture.mode.Load(), "mutations": fixture.mutations.Load()})
	})
	mux.HandleFunc("POST /__fixture/mode/{mode}", func(w http.ResponseWriter, r *http.Request) {
		mode := map[string]int32{"success": 0, "failed": 1, "slow": 2, "storage": 3}
		value, ok := mode[r.PathValue("mode")]
		if !ok {
			w.WriteHeader(400)
			return
		}
		fixture.mode.Store(value)
		w.WriteHeader(204)
	})
	srv := &http.Server{Addr: *address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	mux.HandleFunc("POST /__fixture/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})
	fmt.Println("synthetic_catalog_fixture_ready")
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic("fixture_server_failed")
	}
}
