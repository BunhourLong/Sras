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
