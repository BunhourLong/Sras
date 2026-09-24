package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"sras/internal/service"
)

// Error codes carried in the response envelope.
const (
	codeBadRequest = "bad_request"
	codeNotFound   = "not_found"
	codeConflict   = "conflict"
	codeInternal   = "internal"
)

// errorResponse is the wire shape: {"error": {"code": "...", "message": "..."}}
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes v as the response body.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; all that is left is to record it.
		s.log.Error("write response body", "err", err)
	}
}

// writeError sends the error envelope.
func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

// writeServiceError maps a service error onto a status code and error code.
func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		s.writeError(w, http.StatusNotFound, codeNotFound, "document not found")
	case errors.Is(err, service.ErrInvalidCollection):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "invalid collection name")
	case errors.Is(err, service.ErrInvalidID):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "invalid document id")
	case errors.Is(err, service.ErrConflict):
		s.writeError(w, http.StatusConflict, codeConflict, "revision conflict")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
	}
}
