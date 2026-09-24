package api

import (
	"encoding/json"
	"net/http"

	"sras/internal/service"
)

// maxBodyBytes caps a request body.
const maxBodyBytes = 1 << 20 // 1 MB

// handleGetDocument serves GET /db/{collection}/{id}.
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := s.docs.Get(r.Context(), r.PathValue("collection"), r.PathValue("id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, doc)
}

// handlePutDocument serves PUT /db/{collection}/{id}: 201 on create, 200 on
// replace.
func (s *Server) handlePutDocument(w http.ResponseWriter, r *http.Request) {
	var doc service.Document
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&doc); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "body must be a JSON object")
		return
	}
	saved, created, err := s.docs.Put(r.Context(), r.PathValue("collection"), r.PathValue("id"), doc)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, saved)
}

// handleHealthz serves GET /healthz.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
