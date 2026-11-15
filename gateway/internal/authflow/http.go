package authflow

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Route is mounted behind the panel's session, same-origin and write-marker gates.
func (s *Service) Route(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/connections/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/api/connections/providers":
			write(200, map[string]any{"providers": s.Providers()})
			return true
		case "/api/connections/flows":
			rows, err := s.List()
			if err != nil {
				write(503, map[string]string{"error": "authentication_storage_unavailable"})
			} else {
				write(200, map[string]any{"flows": rows})
			}
			return true
		}
		write(404, map[string]string{"error": "not_found"})
		return true
	}
	if r.Method != http.MethodPost {
		write(405, map[string]string{"error": "method_not_allowed"})
		return true
	}
	var req struct {
		Request
		ID     string `json:"id"`
		Action string `json:"action"`
		Input  Input  `json:"input"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		write(400, map[string]string{"error": "authentication_request_invalid"})
		return true
	}
	var view View
	var err error
	switch r.URL.Path {
	case "/api/connections/start":
		view, err = s.Start(r.Context(), req.Request)
	case "/api/connections/step":
		view, err = s.Step(r.Context(), req.ID, req.Action, req.Input)
	default:
		write(404, map[string]string{"error": "not_found"})
		return true
	}
	if err != nil {
		code := 400
		if err == ErrBusy {
			code = 409
		}
		if err == ErrMissing {
			code = 404
		}
		if err == ErrStorage {
			code = 503
		}
		write(code, map[string]any{"error": err.Error(), "flow": view})
		return true
	}
	write(200, map[string]any{"ok": true, "flow": view})
	return true
}
