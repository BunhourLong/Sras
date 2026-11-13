// Package api exposes the HTTP layer: routing, JSON decoding/encoding, status
// codes and middleware. It holds no business logic and never touches storage.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"sras/internal/service"
)

// DocumentService is the slice of the service layer this package needs.
type DocumentService interface {
	Get(ctx context.Context, collection, id string) (service.Document, error)
	Put(ctx context.Context, collection, id string, doc service.Document) (service.Document, bool, error)
	Create(ctx context.Context, collection string, doc service.Document) (service.Document, error)
	Delete(ctx context.Context, collection, id string, rev *int64) error
}

// Server routes HTTP requests to the document service.
type Server struct {
	docs    DocumentService
	log     *slog.Logger
	timeout time.Duration // bounds every request's context
	mux     *http.ServeMux
	handler http.Handler // the middleware chain wrapping mux
}

// NewServer builds the router and its middleware chain: request ID, access
// log, panic recovery, timeout and body limit, outermost first. timeout
// bounds every request's context; timeout <= 0 means defaultRequestTimeout
// (30s).
func NewServer(docs DocumentService, log *slog.Logger, timeout time.Duration) *Server {
	if log == nil {
		log = slog.Default()
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	// stamp request IDs onto every record this package logs
	log = slog.New(contextHandler{log.Handler()})
	s := &Server{docs: docs, log: log, timeout: timeout, mux: http.NewServeMux()}
	s.routes()
	s.handler = s.withRequestID(s.withAccessLog(s.withRecover(s.withTimeout(s.withBodyLimit(s.mux)))))
	return s
}

// routes registers every handler.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /db/{collection}/{id}", s.handleGetDocument)
	s.mux.HandleFunc("PUT /db/{collection}/{id}", s.handlePutDocument)
	s.mux.HandleFunc("POST /db/{collection}", s.handleCreateDocument)
	s.mux.HandleFunc("DELETE /db/{collection}/{id}", s.handleDeleteDocument)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /openapi.yaml", handleOpenAPI)
	s.mux.HandleFunc("GET /docs", handleDocs)
}

// ServeHTTP applies the middleware chain, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}
