package chok

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// SPA serves the single-page application in fsys under prefix: a file as
// it is, and index.html for a page path that names no file, so the
// application's own router takes the path over. Files under assets/, the
// hashed output of bundlers such as Vite, are cached for a year as
// immutable; everything else, index.html first, is no-cache: checked on
// every use against its ETag, a hash of the content, so an unchanged file
// costs a 304. A missing path whose last segment has a dot is a 404, and
// so is any missing path asked for without text/html in Accept: an API
// client's typo gets the error envelope, not a page.
//
// SPA registers GET (and so HEAD) for prefix and every path below it; a
// prefix of "/" makes it the fallback of the whole site. More specific
// routes, the API and the built-in endpoints, still win. A route for
// every method (method "") below prefix conflicts with SPA's pattern and
// the mux panics: give API routes their methods. The routes are not in
// the OpenAPI document. An fsys without index.html at its root panics.
//
//	//go:embed dist
//	var dist embed.FS
//
//	web, _ := fs.Sub(dist, "dist")
//	chok.SPA(app.Router(), "/", web)
func SPA(r Router, prefix string, fsys fs.FS) {
	if info, err := fs.Stat(fsys, "index.html"); err != nil || !info.Mode().IsRegular() {
		panic(fmt.Sprintf("chok.SPA(%q): fsys has no index.html at its root", prefix))
	}
	s := &spa{fsys: fsys, tags: map[string]spaTag{}}
	p := "/" + strings.Trim(prefix, "/")
	if p == "/" {
		r.Handle(http.MethodGet, "/{rest...}", s)
		return
	}
	r.Handle(http.MethodGet, p, s)
	r.Handle(http.MethodGet, p+"/{rest...}", s)
}

type spa struct {
	fsys fs.FS
	mu   sync.Mutex
	tags map[string]spaTag // path → the ETag of its content
}

// spaTag is a file's ETag, kept while the file's size and modification
// time stay the same: an embed.FS never changes, and a file of an
// os.DirFS that does gets a new tag on its next request.
type spaTag struct {
	etag string
	size int64
	mod  time.Time
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("rest")
	if name != "" && fs.ValidPath(name) && s.serveFile(w, r, name) {
		return
	}
	if name == "" || (!strings.Contains(path.Base(name), ".") && acceptsHTML(r)) {
		if s.serveFile(w, r, "index.html") {
			return
		}
	}
	WriteError(w, r, ErrNotFound)
}

// serveFile serves the file name of fsys with its ETag and cache header,
// and reports false when fsys has no such file.
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := s.fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, ok := f.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(f)
		if err != nil {
			WriteError(w, r, err)
			return true
		}
		content = bytes.NewReader(b)
	}
	etag, err := s.etag(name, info, content)
	if err != nil {
		WriteError(w, r, err)
		return true
	}
	h := w.Header()
	h.Set("ETag", etag)
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, info.ModTime(), content)
	return true
}

// etag is the strong ETag of a file: a hash of its content, computed once
// for each size and modification time the file has.
func (s *spa) etag(name string, info fs.FileInfo, content io.ReadSeeker) (string, error) {
	s.mu.Lock()
	t, ok := s.tags[name]
	s.mu.Unlock()
	if ok && t.size == info.Size() && t.mod.Equal(info.ModTime()) {
		return t.etag, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, content); err != nil {
		return "", err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	t = spaTag{etag: `"` + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:18]) + `"`, size: info.Size(), mod: info.ModTime()}
	s.mu.Lock()
	s.tags[name] = t
	s.mu.Unlock()
	return t.etag, nil
}

func acceptsHTML(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		if strings.Contains(v, "text/html") {
			return true
		}
	}
	return false
}
