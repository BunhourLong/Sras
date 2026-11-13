package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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

func TestCreateDocument(t *testing.T) {
	engine, err := storage.Open(storage.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ts := httptest.NewServer(NewServer(service.NewDocuments(engine, nil), nil))
	defer ts.Close()

	do := func(method, path, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		return resp, got
	}

	// a. create: the server generates the id, a body _id is ignored.
	resp, doc := do("POST", "/db/users", `{"name":"a","_id":"mine"}`) //nolint:bodyclose // resp.Body is closed inside do
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, want 201 (body %v)", resp.StatusCode, doc)
	}
	if doc["_rev"] != float64(1) {
		t.Fatalf("create: _rev %v, want 1", doc["_rev"])
	}
	id, ok := doc["_id"].(string)
	if !ok || id == "mine" {
		t.Fatalf("create: _id %v, want a server-generated id", doc["_id"])
	}
	if doc["_updatedAt"] == nil || doc["name"] != "a" {
		t.Fatalf("create: body %v, want _updatedAt set and name a", doc)
	}
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("create: _id %q: %v", id, err)
	}
	if u.Version() != 7 {
		t.Fatalf("create: _id version %d, want 7", u.Version())
	}
	if loc := resp.Header.Get("Location"); loc != "/db/users/"+id {
		t.Fatalf("create: Location %q, want %q", loc, "/db/users/"+id)
	}

	// b. the Location path reads the document back.
	resp, got := do("GET", resp.Header.Get("Location"), "") //nolint:bodyclose // resp.Body is closed inside do
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read back: status %d, want 200 (body %v)", resp.StatusCode, got)
	}
	if got["_id"] != id || got["name"] != "a" {
		t.Fatalf("read back: body %v, want _id %s and name a", got, id)
	}

	// c. further creates get distinct, strictly ascending UUIDv7 ids.
	ids := []string{id}
	for i := 0; i < 3; i++ {
		resp, d := do("POST", "/db/users", `{"name":"b"}`) //nolint:bodyclose // resp.Body is closed inside do
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: status %d, want 201 (body %v)", i, resp.StatusCode, d)
		}
		next, ok := d["_id"].(string)
		if !ok {
			t.Fatalf("create %d: _id %v, want a string", i, d["_id"])
		}
		ids = append(ids, next)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("ids not strictly ascending: %s then %s", ids[i-1], ids[i])
		}
	}

	// d. the same error cases as PUT.
	steps := []struct {
		name, path, body string
		status           int
	}{
		{"bad collection", "/db/Users", `{}`, 400},
		{"not an object", "/db/users", `[1]`, 400},
		{"null body", "/db/users", `null`, 400},
		{"bad rev", "/db/users", `{"_rev":"x"}`, 400},
		{"stale rev", "/db/users", `{"_rev":3}`, 409},
		{"rev 0 creates", "/db/users", `{"_rev":0}`, 201},
	}
	for _, s := range steps {
		resp, _ := do("POST", s.path, s.body) //nolint:bodyclose // resp.Body is closed inside do
		if resp.StatusCode != s.status {
			t.Fatalf("%s: status %d, want %d", s.name, resp.StatusCode, s.status)
		}
	}

	// e. the generated id works with the existing endpoints.
	resp, got = do("PUT", "/db/users/"+ids[0], `{"_rev":1}`) //nolint:bodyclose // resp.Body is closed inside do
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: status %d, want 200 (body %v)", resp.StatusCode, got)
	}
	if got["_rev"] != float64(2) {
		t.Fatalf("put: _rev %v, want 2", got["_rev"])
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
