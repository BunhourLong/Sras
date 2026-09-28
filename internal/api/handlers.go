package api

import (
	_ "embed"
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

//go:embed openapi.yaml
var openAPISpec []byte

// handleOpenAPI serves GET /openapi.yaml.
func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(openAPISpec)
}

// docsPage renders Swagger UI against /openapi.yaml.
// ponytail: Swagger UI comes from a CDN, so /docs needs internet; vendor it if that matters.
const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Sras API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head><body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/openapi.yaml", dom_id: "#ui"})</script>
</body></html>`

// handleDocs serves GET /docs.
func handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(docsPage))
}
