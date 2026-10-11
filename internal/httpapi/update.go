package httpapi

import "net/http"

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	status := s.updates.Check(r.Context(), r.URL.Query().Get("force") == "1")
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    status,
	})
}
