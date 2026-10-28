package chok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const spaIndex = "<!doctype html><title>app</title>"

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte(spaIndex)},
		"app.js":              {Data: []byte("console.log('app')")},
		"assets/main-1a2b.js": {Data: []byte("export const x = 1")},
	}
}

// spaDo sends method target with header pairs to h.
func spaDo(h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSPA_ServesFiles: a file comes as it is, with its type, a content
// ETag and no-cache; under assets/ it is immutable for a year; ETags give
// 304s, ranges 206s, HEAD no body.
func TestSPA_ServesFiles(t *testing.T) {
	a := newTestApp(t)
	SPA(a.Router(), "/", spaFS())
	h := a.Handler()

	rec := spaDo(h, http.MethodGet, "/app.js")
	etag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('app')" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		rec.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("app.js: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec := spaDo(h, http.MethodGet, "/app.js", "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d %q", rec.Code, rec.Body)
	}
	if rec := spaDo(h, http.MethodGet, "/app.js", "Range", "bytes=0-6"); rec.Code != http.StatusPartialContent || rec.Body.String() != "console" {
		t.Fatalf("Range: %d %q", rec.Code, rec.Body)
	}
	if rec := spaDo(h, http.MethodHead, "/app.js"); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Fatalf("HEAD: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec = spaDo(h, http.MethodGet, "/assets/main-1a2b.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || rec.Header().Get("ETag") == "" {
		t.Fatalf("assets: %d %v", rec.Code, rec.Header())
	}
	rec = spaDo(h, http.MethodGet, "/index.html")
	if rec.Code != http.StatusOK || rec.Body.String() != spaIndex || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index.html: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
}

// TestSPA_MissingPaths: a page path gets index.html when the request
// accepts HTML; a file-like path, or a request that does not accept HTML,
// gets the 404 envelope; a directory is never listed.
func TestSPA_MissingPaths(t *testing.T) {
	a := newTestApp(t)
	SPA(a.Router(), "/", spaFS())
	h := a.Handler()
	const html = "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8"
	for _, c := range []struct {
		target, accept string
		index          bool
	}{
		{"/", "", true},
		{"/users/42", html, true},
		{"/users/42", "application/json", false},
		{"/users/42", "", false},
		{"/missing.js", html, false},
		{"/assets/missing-9z.css", html, false},
		{"/assets/", html, true},
		{"/assets", "application/json", false},
	} {
		rec := spaDo(h, http.MethodGet, c.target, "Accept", c.accept)
		switch {
		case c.index && (rec.Code != http.StatusOK || rec.Body.String() != spaIndex || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") == ""):
			t.Errorf("%s (Accept %q): want index.html, got %d %q %v", c.target, c.accept, rec.Code, rec.Body, rec.Header())
		case !c.index && (rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "NotFound")):
			t.Errorf("%s (Accept %q): want the 404 envelope, got %d %q", c.target, c.accept, rec.Code, rec.Body)
		}
	}
}

// TestSPA_RoutesStayInFront: the API's routes are more specific than the
// fallback, and a method the API does not serve is still the mux's 405.
func TestSPA_RoutesStayInFront(t *testing.T) {
	a := newTestApp(t)
	GET(a.Group("/api"), "/ping", func(context.Context, *None) (string, error) { return "pong", nil })
	SPA(a.Router(), "/", spaFS())
	h := a.Handler()
	if rec := spaDo(h, http.MethodGet, "/api/ping", "Accept", "text/html"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("API route: %d %q", rec.Code, rec.Body)
	}
	if rec := spaDo(h, http.MethodPost, "/api/ping"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a GET route: %d %q", rec.Code, rec.Body)
	}
}

// TestSPA_UnderAPrefix: below a group and a prefix, with or without its
// slashes, SPA serves only its own paths.
func TestSPA_UnderAPrefix(t *testing.T) {
	a := newTestApp(t)
	SPA(a.Group("/admin"), "/ui/", spaFS())
	h := a.Handler()
	for target, want := range map[string]string{
		"/admin/ui":          spaIndex,
		"/admin/ui/":         spaIndex,
		"/admin/ui/orders/7": spaIndex,
		"/admin/ui/app.js":   "console.log('app')",
	} {
		if rec := spaDo(h, http.MethodGet, target, "Accept", "text/html"); rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Body)
		}
	}
	for _, target := range []string{"/app.js", "/admin/other", "/orders/7"} {
		if rec := spaDo(h, http.MethodGet, target, "Accept", "text/html"); rec.Code != http.StatusNotFound {
			t.Errorf("%s outside the prefix: %d %q", target, rec.Code, rec.Body)
		}
	}
	if rec := spaDo(h, http.MethodGet, "/admin/ui/assets/main-1a2b.js"); rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("assets below the prefix: %v", rec.Header())
	}
}

// TestSPA_ChangedFileGetsANewETag: a file of an os.DirFS that changes
// gets a new ETag on its next request.
func TestSPA_ChangedFileGetsANewETag(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	a := newTestApp(t)
	SPA(a.Router(), "/", os.DirFS(dir))
	h := a.Handler()
	first := spaDo(h, http.MethodGet, "/").Header().Get("ETag")
	if again := spaDo(h, http.MethodGet, "/").Header().Get("ETag"); first == "" || again != first {
		t.Fatalf("unchanged file: %q then %q", first, again)
	}
	write("three")
	if rec := spaDo(h, http.MethodGet, "/"); rec.Body.String() != "three" || rec.Header().Get("ETag") == first {
		t.Fatalf("changed file: %q %q (was %q)", rec.Body, rec.Header().Get("ETag"), first)
	}
}

func TestSPA_NoIndexPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "index.html") {
			t.Fatalf("recover() = %v", r)
		}
	}()
	SPA(newTestApp(t).Router(), "/", fstest.MapFS{"app.js": {Data: []byte("x")}})
}
