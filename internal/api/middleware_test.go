package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"sras/internal/service"
	"sras/internal/storage"
)

// syncBuffer is a mutex-guarded bytes.Buffer: server goroutines write log
// records into it while the test reads them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every line of the buffer as a parsed JSON log record.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// newLoggedServer builds a server over a real engine in a temp dir and a
// JSON logger writing to a guarded buffer, like handlers_test.go does. docs
// nil means the real document service. The caller closes the server; the
// engine is closed on cleanup.
func newLoggedServer(t *testing.T, docs DocumentService) (*httptest.Server, *syncBuffer) {
	t.Helper()
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	if docs == nil {
		docs = service.NewDocuments(engine, log)
	}
	return httptest.NewServer(NewServer(docs, log)), buf
}

// envCode returns the error envelope's code, failing if the body has none.
func envCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", body)
	}
	c, _ := e["code"].(string)
	return c
}

// TestRequestID checks the request-ID middleware: a missing or invalid
// incoming X-Request-ID is replaced by a generated UUID, a valid one is
// echoed unchanged, and the response always carries the ID.
func TestRequestID(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ts := httptest.NewServer(NewServer(service.NewDocuments(engine, nil), nil))
	defer ts.Close()

	do := func(header string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
		if header != "" {
			req.Header.Set("X-Request-ID", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.Header.Get("X-Request-ID")
	}

	// a. no header: the response carries a generated UUID.
	id := do("")
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("no header: X-Request-ID %q: %v", id, err)
	}

	// b. a valid incoming id is echoed unchanged.
	if id := do("abc-123"); id != "abc-123" {
		t.Fatalf("valid header: echoed %q, want abc-123", id)
	}

	// c. an invalid incoming id (200 bytes, or with a space) is replaced.
	for _, in := range []string{strings.Repeat("x", 200), "a b"} {
		got := do(in)
		if got == in {
			t.Fatalf("invalid header: echoed %q back, want a generated UUID", in)
		}
		if _, err := uuid.Parse(got); err != nil {
			t.Fatalf("invalid header %q: X-Request-ID %q: %v", in, got, err)
		}
	}
}

// TestAccessLog checks that every request gets one access-log record with
// the method, matched route pattern, status, bytes and duration, and the
// request ID of the response header.
func TestAccessLog(t *testing.T) {
	ts, buf := newLoggedServer(t, nil)

	resp, err := http.Get(ts.URL + "/db/users/x") // 404
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Request-ID")
	ts.Close() // wait for the handler chain to finish before reading the log

	records := buf.records(t)
	if len(records) != 1 {
		t.Fatalf("%d log records, want 1: %v", len(records), records)
	}
	rec := records[0]
	if rec["msg"] != "request" {
		t.Fatalf("msg %v, want request", rec["msg"])
	}
	if rec["method"] != "GET" {
		t.Fatalf("method %v, want GET", rec["method"])
	}
	if rec["pattern"] != "GET /db/{collection}/{id}" {
		t.Fatalf("pattern %v, want the matched route pattern", rec["pattern"])
	}
	if rec["status"] != float64(http.StatusNotFound) {
		t.Fatalf("status %v, want 404", rec["status"])
	}
	if n, _ := rec["bytes"].(float64); n <= 0 {
		t.Fatalf("bytes %v, want > 0", rec["bytes"])
	}
	if _, ok := rec["duration"]; !ok {
		t.Fatal("no duration in the access-log record")
	}
	if rec["request_id"] != reqID {
		t.Fatalf("request_id %v, want %q from the response header", rec["request_id"], reqID)
	}
}

// TestRecover checks that a panicking handler produces a 500 in the error
// envelope without the panic text, a panic log record with a stack and the
// request ID, an access-log line with status 500, and a live server.
func TestRecover(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	s := NewServer(service.NewDocuments(engine, log), log)
	s.mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	ts := httptest.NewServer(s)

	resp, err := http.Get(ts.URL + "/panic")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (body %s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", raw, err)
	}
	if c := envCode(t, body); c != codeInternal {
		t.Fatalf("code %q, want internal", c)
	}
	if strings.Contains(string(raw), "boom") {
		t.Fatal("response body contains the panic value")
	}
	reqID := resp.Header.Get("X-Request-ID")

	// the server is not dead: a normal request still works.
	resp2, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after panic: status %d, want 200", resp2.StatusCode)
	}
	ts.Close() // wait for the log writes before reading the buffer

	var panicRec, accessRec map[string]any
	for _, rec := range buf.records(t) {
		switch rec["msg"] {
		case "panic":
			panicRec = rec
		case "request":
			// the panicked request's line, not the healthz one
			if rec["request_id"] == reqID {
				accessRec = rec
			}
		}
	}
	if panicRec == nil {
		t.Fatal("no panic record in the log")
	}
	if stack, _ := panicRec["stack"].(string); stack == "" {
		t.Fatal("panic record has no stack")
	}
	if panicRec["request_id"] != reqID {
		t.Fatalf("panic record request_id %v, want %q", panicRec["request_id"], reqID)
	}
	if accessRec == nil {
		t.Fatal("no access-log record for the panicked request")
	}
	if accessRec["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("access-log status %v, want 500", accessRec["status"])
	}
}

// TestRequestTimeout checks that the timeout middleware gives every request
// context a deadline close to requestTimeout.
func TestRequestTimeout(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	s := NewServer(service.NewDocuments(engine, nil), nil)
	s.mux.HandleFunc("GET /timeout", func(w http.ResponseWriter, r *http.Request) {
		dl, ok := r.Context().Deadline()
		if !ok {
			http.Error(w, "no deadline in the request context", http.StatusInternalServerError)
			return
		}
		if until := time.Until(dl); until <= 0 || until > requestTimeout {
			http.Error(w, fmt.Sprintf("deadline in %v, want within %v", until, requestTimeout), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/timeout")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, raw)
	}
}

// TestBodyLimit checks that bodies over maxBodyBytes are rejected with 400
// bad_request by the middleware limit, for both PUT and POST.
func TestBodyLimit(t *testing.T) {
	ts, _ := newLoggedServer(t, nil)
	defer ts.Close()

	// a valid JSON object over the limit, so the limit is what fails it
	body := `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`

	do := func(method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw)
	}

	for _, step := range []struct{ method, path string }{
		{"PUT", "/db/users/1"},
		{"POST", "/db/users"},
	} {
		status, raw := do(step.method, step.path)
		if status != http.StatusBadRequest {
			t.Fatalf("%s %s: status %d, want 400 (body %s)", step.method, step.path, status, raw)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", step.method, step.path, raw, err)
		}
		if c := envCode(t, doc); c != codeBadRequest {
			t.Fatalf("%s %s: code %q, want bad_request", step.method, step.path, c)
		}
	}
}

// failingService is a DocumentService whose every method fails with a plain
// error, to exercise the internal-error path of writeServiceError.
type failingService struct{}

func (failingService) Get(context.Context, string, string) (service.Document, error) {
	return nil, errors.New("boom")
}

func (failingService) Put(context.Context, string, string, service.Document) (service.Document, bool, error) {
	return nil, false, errors.New("boom")
}

func (failingService) Create(context.Context, string, service.Document) (service.Document, error) {
	return nil, errors.New("boom")
}

func (failingService) Delete(context.Context, string, string, *int64) error {
	return errors.New("boom")
}

// TestServiceErrorLog checks that the 500 path of writeServiceError logs the
// failure with the request ID and never leaks the error text to the client.
func TestServiceErrorLog(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	ts := httptest.NewServer(NewServer(failingService{}, log))

	resp, err := http.Get(ts.URL + "/db/users/1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (body %s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", raw, err)
	}
	if c := envCode(t, body); c != codeInternal {
		t.Fatalf("code %q, want internal", c)
	}
	if strings.Contains(string(raw), "boom") {
		t.Fatal("response leaks the internal error text")
	}
	reqID := resp.Header.Get("X-Request-ID")
	ts.Close() // wait for the log write before reading the buffer

	for _, rec := range buf.records(t) {
		if rec["msg"] == "request failed" {
			if rec["request_id"] != reqID {
				t.Fatalf("log request_id %v, want %q", rec["request_id"], reqID)
			}
			return
		}
	}
	t.Fatal("no 'request failed' record in the log")
}
