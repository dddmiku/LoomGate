package taskengine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Handler must be mounted behind the gateway's internal-management boundary.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, code int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(value)
	}
	decode := func(w http.ResponseWriter, r *http.Request, value any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		raw, err := io.ReadAll(r.Body)
		raw = bytes.TrimSpace(raw)
		if err != nil || len(raw) == 0 || raw[0] != '{' {
			respond(w, 400, map[string]any{"error": "invalid_request"})
			return false
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
			respond(w, 400, map[string]any{"error": "invalid_request"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /workbench", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"providers": e.Providers(), "schedules": e.Schedules(), "runs": e.Runs()})
	})
	mux.HandleFunc("GET /workbench/task/log", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		provider := r.URL.Query().Get("provider")
		res, err := e.TaskLogFor(provider, key)
		if err != nil {
			respond(w, 400, map[string]any{"ok": false, "lines": []string{}, "error": "invalid_request", "message": "任务标识无效。"})
			return
		}
		lines := res.Lines
		if lines == nil {
			lines = []string{}
		}
		body := map[string]any{"ok": true, "key": key, "lines": lines, "count": len(lines), "running": res.Running}
		if res.Message != "" {
			body["message"] = res.Message
		}
		if !res.EndedAt.IsZero() {
			body["ended_at"] = res.EndedAt.UTC().Format(time.RFC3339)
		}
		respond(w, 200, body)
	})
	mux.HandleFunc("GET /workbench/catalog", func(w http.ResponseWriter, r *http.Request) {
		ids := []string{}
		if s := r.URL.Query().Get("accounts"); s != "" {
			ids = strings.Split(s, ",")
		}
		out, err := e.CatalogDetails(r.Context(), r.URL.Query().Get("provider"), Scope{AccountIDs: ids}, r.URL.Query().Get("task"))
		if err != nil {
			respond(w, 502, map[string]any{"error": "catalog_unavailable"})
			return
		}
		respond(w, 200, out)
	})
	mux.HandleFunc("POST /workbench/run", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if !decode(w, r, &req) {
			return
		}
		req.Trigger = "manual"
		req.ScheduleID = ""
		out, err := e.Submit(req)
		if err != nil {
			respond(w, 409, map[string]any{"error": "run_rejected"})
			return
		}
		respond(w, 202, map[string]any{"ok": true, "run": out})
	})
	mux.HandleFunc("POST /workbench/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &req) {
			return
		}
		respond(w, 202, map[string]any{"ok": true, "cancel_requested": e.Cancel(req.ID)})
	})
	mux.HandleFunc("POST /workbench/schedule", func(w http.ResponseWriter, r *http.Request) {
		var s Schedule
		if !decode(w, r, &s) {
			return
		}
		out, err := e.SaveSchedule(s)
		if err != nil {
			respond(w, 400, map[string]any{"error": "schedule_not_saved"})
			return
		}
		respond(w, 200, map[string]any{"ok": true, "schedule": out})
	})
	mux.HandleFunc("POST /workbench/schedule/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &req) {
			return
		}
		if e.DeleteSchedule(req.ID) != nil {
			respond(w, 404, map[string]any{"error": "schedule_not_found"})
			return
		}
		respond(w, 200, map[string]any{"ok": true})
	})
	return mux
}
