package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIExport_JSONAndYAML(t *testing.T) {
	spec := `{"openapi":"3.0.3","info":{"title":"Blog API","version":"1.0.0"},` +
		`"paths":{"/api/v1/posts":{},"/auth/login":{}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swagger/doc.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(spec))
	}))
	defer srv.Close()

	dir := t.TempDir()
	run := func(out string) string {
		t.Helper()
		cmd := openapiCmd()
		var sb strings.Builder
		cmd.SetOut(&sb)
		cmd.SetErr(&sb)
		cmd.SetArgs([]string{"export", "--url", srv.URL + "/swagger/doc.json", "--out", out})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("export %s: %v\n%s", out, err, sb.String())
		}
		return sb.String()
	}

	jsonOut := filepath.Join(dir, "openapi.json")
	msg := run(jsonOut)
	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"/api/v1/posts"`) {
		t.Fatalf("json export must carry the paths: %s", raw)
	}
	if !strings.Contains(msg, "Blog API") || !strings.Contains(msg, "2 paths") {
		t.Fatalf("summary line must identify the spec, got %q", msg)
	}

	yamlOut := filepath.Join(dir, "openapi.yaml")
	run(yamlOut)
	rawYAML, err := os.ReadFile(yamlOut)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := yaml.Unmarshal(rawYAML, &round); err != nil {
		t.Fatalf("yaml export must parse: %v", err)
	}
	if round["openapi"] != "3.0.3" {
		t.Fatalf("yaml export lost the spec shape: %v", round)
	}
}

func TestOpenAPIExport_UsefulErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	cmd := openapiCmd()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs([]string{"export", "--url", srv.URL + "/swagger/doc.json", "--out", filepath.Join(t.TempDir(), "x.json")})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a 404 must surface with guidance, got %v", err)
	}
}
