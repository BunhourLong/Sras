package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sras/internal/service"
	"sras/internal/storage"
)

func TestDocumentCRUD(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ts := httptest.NewServer(NewServer(service.NewDocuments(engine, nil), nil))
	defer ts.Close()

	steps := []struct {
		name, method, path, body string
		status                   int
		rev                      float64 // checked when non-zero
	}{
		{"healthz", "GET", "/healthz", "", 200, 0},
		{"get missing", "GET", "/db/users/1", "", 404, 0},
		{"create", "PUT", "/db/users/1", `{"name":"a"}`, 201, 1},
		{"get", "GET", "/db/users/1", "", 200, 1},
		{"replace", "PUT", "/db/users/1", `{"name":"b","_rev":1}`, 200, 2},
		{"stale rev", "PUT", "/db/users/1", `{"name":"c","_rev":1}`, 409, 0},
		{"missing rev on existing", "PUT", "/db/users/1", `{"name":"c"}`, 409, 0},
		{"bad rev", "PUT", "/db/users/1", `{"_rev":"x"}`, 400, 0},
		{"not an object", "PUT", "/db/users/2", `[1]`, 400, 0},
		{"null body", "PUT", "/db/users/2", `null`, 400, 0},
		{"bad collection", "PUT", "/db/Users/1", `{}`, 400, 0},
		{"get after replace", "GET", "/db/users/1", "", 200, 2},
		{"delete stale rev", "DELETE", "/db/users/1?rev=1", "", 409, 0},
		{"delete bad rev", "DELETE", "/db/users/1?rev=x", "", 400, 0},
		{"delete", "DELETE", "/db/users/1?rev=2", "", 204, 0},
		{"get deleted", "GET", "/db/users/1", "", 404, 0},
		{"delete again", "DELETE", "/db/users/1", "", 404, 0},
		{"delete bad collection", "DELETE", "/db/Users/1", "", 400, 0},
		{"recreate after delete", "PUT", "/db/users/1", `{"name":"d"}`, 201, 1},
		{"delete without rev", "DELETE", "/db/users/1", "", 204, 0},
	}
	for _, s := range steps {
		req, _ := http.NewRequest(s.method, ts.URL+s.path, strings.NewReader(s.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()

		if resp.StatusCode != s.status {
			t.Fatalf("%s: status %d, want %d (body %v)", s.name, resp.StatusCode, s.status, body)
		}
		if s.rev != 0 {
			if body["_rev"] != s.rev || body["_id"] != "1" || body["_updatedAt"] == nil {
				t.Fatalf("%s: body %v, want _id 1 and _rev %v", s.name, body, s.rev)
			}
		}
	}
}

func TestDeleteIfMatch(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ts := httptest.NewServer(NewServer(service.NewDocuments(engine, nil), nil))
	defer ts.Close()

	do := func(method, ifMatch, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+"/db/users/1", strings.NewReader(body))
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := do("PUT", "", `{}`); got != 201 {
		t.Fatalf("create: status %d, want 201", got)
	}
	if got := do("DELETE", `"2"`, ""); got != 409 {
		t.Fatalf("stale If-Match: status %d, want 409", got)
	}
	if got := do("DELETE", `"1"`, ""); got != 204 {
		t.Fatalf("If-Match: status %d, want 204", got)
	}
}

// Regression: after a restart, a PUT without _rev on an existing document
// must conflict instead of silently re-creating it at rev 1.
func TestRevSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	put := func(engine storage.Engine, body string) int {
		t.Helper()
		ts := httptest.NewServer(NewServer(service.NewDocuments(engine, nil), nil))
		defer ts.Close()
		req, _ := http.NewRequest("PUT", ts.URL+"/db/users/1", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	engine, err := storage.Open(storage.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := put(engine, `{"name":"a"}`); got != 201 {
		t.Fatalf("create: status %d, want 201", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	engine, err = storage.Open(storage.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if got := put(engine, `{"name":"b"}`); got != 409 {
		t.Fatalf("put without _rev after restart: status %d, want 409", got)
	}
	if got := put(engine, `{"name":"b","_rev":1}`); got != 200 {
		t.Fatalf("put with _rev 1 after restart: status %d, want 200", got)
	}
}
