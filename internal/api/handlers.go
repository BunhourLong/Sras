package api

import "net/http"

// handleGetDocument serves GET /db/{collection}/{id}.
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := s.docs.Get(r.Context(), r.PathValue("collection"), r.PathValue("id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, doc)
}
