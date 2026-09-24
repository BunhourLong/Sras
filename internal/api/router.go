// Package api exposes the HTTP layer: routing, JSON decoding/encoding, status
// codes and middleware. It holds no business logic and never touches storage.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"sras/internal/service"
)

// DocumentService is the slice of the service layer this package needs.
type DocumentService interface {
	Get(ctx context.Context, collection, id string) (service.Document, error)
}

// Server routes HTTP requests to the document service.
type Server struct {
	docs DocumentService
	log  *slog.Logger
	mux  *http.ServeMux
}

// NewServer builds the router.
func NewServer(docs DocumentService, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{docs: docs, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// routes registers every handler. Only the document read is wired up so far.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /db/{collection}/{id}", s.handleGetDocument)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
