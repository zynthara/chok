package chok

import (
	"net/http"
	"net/http/pprof"

	"github.com/zynthara/chok/v3/internal/openapi"
)

// openAPIDocument renders the OpenAPI 3.1 document of the typed routes.
func (a *App) openAPIDocument() ([]byte, error) {
	c := a.base.HTTP.OpenAPI
	docs := a.routes.all()
	ops := make([]openapi.Operation, 0, len(docs))
	for _, d := range docs {
		op := openapi.Operation{
			Method: d.method, Path: d.path,
			Summary: d.cfg.summary, Tags: d.cfg.tags, Status: d.status,
			Public: d.cfg.public, Deprecated: d.cfg.deprecated,
			Request: d.req, Response: d.resp,
		}
		if d.list {
			op.ListItem, op.Filters = d.item, d.filters
		}
		ops = append(ops, op)
	}
	version := c.Version
	if version == "" {
		version = a.version
	}
	cookie := ""
	if p := a.cookieAuth.Load(); p != nil {
		cookie = *p
	}
	return openapi.Build(openapi.Info{Title: c.Title, Version: version, BearerAuth: c.BearerAuth, CookieAuth: cookie}, ops)
}

// identity documents its session cookie through the internal hook: a
// battery cannot reach the App, and the document has no public option
// for it.
func init() {
	openapi.UseCookieAuth = func(router any, cookie string) {
		if r, ok := router.(Router); ok {
			if a := appOf(r); a != nil {
				a.cookieAuth.Store(&cookie)
			}
		}
	}
}

func (a *App) mountOpenAPI() {
	c := a.base.HTTP.OpenAPI
	if !c.Enabled {
		return
	}
	path := c.Path
	if path == "" {
		path = "/openapi.json"
	}
	a.handleBuiltin(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		doc, err := a.openAPIDocument()
		if err != nil {
			a.writeError(w, r, ErrInternal.Wrap(err))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(doc)
	})
	if c.DocsPath == "" {
		return
	}
	title := c.Title
	if title == "" {
		title = "API"
	}
	page := openapi.UI(title, path)
	a.handleBuiltin(http.MethodGet, c.DocsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}

// mountDebug serves net/http/pprof under /debug/pprof/ when app.debug is
// on, the goroutineleak profile included.
func (a *App) mountDebug() {
	if !a.base.App.Debug {
		return
	}
	a.handleBuiltin(http.MethodGet, "/debug/pprof/", pprof.Index)
	a.handleBuiltin(http.MethodGet, "/debug/pprof/cmdline", pprof.Cmdline)
	a.handleBuiltin(http.MethodGet, "/debug/pprof/profile", pprof.Profile)
	a.handleBuiltin(http.MethodGet, "/debug/pprof/symbol", pprof.Symbol)
	a.handleBuiltin(http.MethodPost, "/debug/pprof/symbol", pprof.Symbol)
	a.handleBuiltin(http.MethodGet, "/debug/pprof/trace", pprof.Trace)
	a.handleBuiltin(http.MethodGet, "/debug/pprof/goroutineleak", pprof.Handler("goroutineleak").ServeHTTP)
}
