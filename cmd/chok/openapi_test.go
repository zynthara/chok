package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
)

type exportedThing struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type thingID struct {
	ID string `path:"id" validate:"required"`
}

// serveApp starts a chok App through choktest and serves its handler on
// a random port, as a deployed application serves /openapi.json.
func serveApp(t *testing.T) *httptest.Server {
	t.Helper()
	var app *chok.App
	choktest.App(t, chok.Base{}, func(a *chok.App) {
		app = a
		chok.GET(a.Router(), "/things/{id}", func(_ context.Context, req *thingID) (*exportedThing, error) {
			return &exportedThing{ID: req.ID}, nil
		}, chok.Summary("Get a thing"))
	})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func decodeJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return v
}

// TestOpenAPIExport_JSONAndYAML: export fetches the document of a running
// App — at the address alone, the default path added — and prints it as
// indented JSON, or writes it to --out as JSON or YAML with the same
// content.
func TestOpenAPIExport_JSONAndYAML(t *testing.T) {
	srv := serveApp(t)
	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var served any
	if err := json.UnmarshalRead(resp.Body, &served); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	for _, arg := range []string{srv.URL + "/openapi.json", srv.URL, host} {
		out, err := run(t, "openapi", "export", arg)
		if err != nil {
			t.Fatalf("export %s: %v", arg, err)
		}
		if !strings.HasPrefix(out, "{\n  \"openapi\": \"3.1.0\",\n") || !reflect.DeepEqual(decodeJSON(t, []byte(out)), served) {
			t.Fatalf("export %s printed:\n%.300s", arg, out)
		}
	}

	dir := t.TempDir()
	jsonOut, yamlOut := filepath.Join(dir, "api.json"), filepath.Join(dir, "api.yaml")
	if _, err := run(t, "openapi", "export", srv.URL, "--out", jsonOut); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(jsonOut)
	if !reflect.DeepEqual(decodeJSON(t, raw), served) {
		t.Fatalf("api.json differs from the served document:\n%.300s", raw)
	}
	if _, err := run(t, "openapi", "export", srv.URL, "-o", yamlOut); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(yamlOut)
	var fromYAML any
	if err := yaml.Unmarshal(raw, &fromYAML); err != nil {
		t.Fatalf("api.yaml: %v\n%s", err, raw)
	}
	if !strings.HasPrefix(string(raw), "openapi: 3.1.0\n") || !reflect.DeepEqual(fromYAML, served) {
		t.Fatalf("api.yaml differs from the served document:\n%.300s", raw)
	}
	if !strings.Contains(string(raw), "/things/{id}:") {
		t.Fatalf("api.yaml lacks the route:\n%s", raw)
	}
}

// TestOpenAPIExport_UsefulErrors: a 404, an answer that is not an OpenAPI
// document, a bad address and an application that is not running fail
// with a message saying so, and an existing --out file survives a failed
// export untouched.
func TestOpenAPIExport_UsefulErrors(t *testing.T) {
	srv := serveApp(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"up"}`))
	}))
	t.Cleanup(other.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	out := filepath.Join(t.TempDir(), "keep.json")
	if err := os.WriteFile(out, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ arg, want string }{
		{srv.URL + "/docs.json", "404 Not Found (is http.openapi.enabled off"},
		{other.URL + "/healthz", "did not return an OpenAPI document"},
		{"ftp://example.com/openapi.json", "not an http(s) address"},
		{closed.URL, "is the application running?"},
	} {
		_, err := run(t, "openapi", "export", c.arg, "--out", out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("export %s: %v, want %q", c.arg, err, c.want)
		}
	}
	if raw, _ := os.ReadFile(out); string(raw) != "previous" {
		t.Fatalf("a failed export changed --out: %q", raw)
	}
	if _, err := run(t, "openapi", "export", srv.URL, "--out", out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); !strings.Contains(string(raw), `"openapi": "3.1.0"`) {
		t.Fatalf("a successful export replaces --out: %.100s", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Fatalf("temporary files left beside --out: %v", entries)
	}
}
