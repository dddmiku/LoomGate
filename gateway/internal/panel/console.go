package panel

import (
	"net/http"
	"strings"
)

func (s *Server) consoleAsset(w http.ResponseWriter, path string) {
	body, err := readAsset(s.assets, strings.TrimPrefix(path, "/"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset_not_found"})
		return
	}
	kind := "application/javascript; charset=utf-8"
	if strings.HasSuffix(path, ".css") {
		kind = "text/css; charset=utf-8"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeBytes(w, http.StatusOK, []byte(body), kind)
}
