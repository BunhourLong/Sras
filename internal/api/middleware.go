package api

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

// requestTimeout bounds every request handled by the API.
// shortcut: fixed value until 2.4 makes it configurable.
const requestTimeout = 30 * time.Second

// requestIDKey is the unexported context key under which withRequestID
// stores the request ID.
type requestIDKey struct{}

// requestIDFromContext returns the ID stored by withRequestID, if any.
func requestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

// validRequestID reports whether v is 1-128 bytes of printable ASCII.
func validRequestID(v string) bool {
	if len(v) < 1 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7E {
			return false
		}
	}
	return true
}

// withRequestID gives every request an ID: the incoming X-Request-ID header
// when it is 1-128 bytes of printable ASCII, a fresh UUID otherwise. The ID
// is echoed in the response header and stored in the request context.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		// echoed before next so even a panic response carries it
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// contextHandler adds the request ID from the context to every record, so
// log lines can be traced to their request.
type contextHandler struct {
	slog.Handler
}

// Enabled delegates to the wrapped handler.
func (h contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Handler.Enabled(ctx, l)
}

// Handle injects the request ID when the context carries one.
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := requestIDFromContext(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs keeps the request-ID wrapper around the returned handler.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup keeps the request-ID wrapper around the returned handler.
func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// statusRecorder records the status code, bytes written and whether a header
// was sent, for the access log and panic recovery.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// WriteHeader records the status and passes it through once.
func (w *statusRecorder) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write counts body bytes and defaults the status to 200.
func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// withAccessLog writes one line per request after it finishes.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// pattern from the mux, not r.Pattern, which needs Go 1.23
		_, pattern := s.mux.Handler(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"pattern", pattern,
			"status", rec.status,
			"duration", time.Since(start),
			"bytes", rec.bytes)
	})
}

// withRecover turns handler panics into a logged 500 in the error envelope,
// except http.ErrAbortHandler, which the server expects to see.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.ErrorContext(r.Context(), "panic", "panic", v, "stack", string(debug.Stack()))
				// a 500 is only writable when no headers went out yet
				if rec, ok := w.(*statusRecorder); !ok || !rec.wroteHeader {
					s.writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withTimeout bounds every request's context at requestTimeout.
func (s *Server) withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withBodyLimit caps every request body at maxBodyBytes.
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}
