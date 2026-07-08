package web

import (
	"net/http"

	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
)

// The verb helpers fuse routing and the typed binding layer into one
// line — the shortest path from a Routes callback to a bound,
// documented endpoint:
//
//	chok.Routes(func(r chok.Router, k chok.Kernel) error {
//	    web.GET(r, "/ping", func(ctx context.Context, _ *struct{}) (string, error) {
//	        return "pong", nil
//	    })
//	    web.POST(r, "/posts", createPost) // func(ctx, *CreateReq) (*Post, error)
//	    return nil
//	})
//
// Each call is exactly r.Handle(method, pattern,
// handler.HandleRequest(fn, opts...), mw...) — same binding, error
// mapping, OpenAPI registration and middleware semantics, nothing
// bypassed. Middleware still rides r.Group; per-route wrapping stays
// on r.Handle.
//
// DELETE takes an action (no response body, 204 by default) because
// that is the REST norm and the common case; a DELETE that must return
// a body uses r.Handle with handler.HandleRequest directly.

// GET registers a typed GET endpoint.
func GET[T any, R any](r kernel.Router, pattern string, fn handler.HandlerFunc[T, R], opts ...handler.HandleOption) {
	r.Handle(http.MethodGet, pattern, handler.HandleRequest(fn, opts...))
}

// POST registers a typed POST endpoint.
func POST[T any, R any](r kernel.Router, pattern string, fn handler.HandlerFunc[T, R], opts ...handler.HandleOption) {
	r.Handle(http.MethodPost, pattern, handler.HandleRequest(fn, opts...))
}

// PUT registers a typed PUT endpoint.
func PUT[T any, R any](r kernel.Router, pattern string, fn handler.HandlerFunc[T, R], opts ...handler.HandleOption) {
	r.Handle(http.MethodPut, pattern, handler.HandleRequest(fn, opts...))
}

// PATCH registers a typed PATCH endpoint.
func PATCH[T any, R any](r kernel.Router, pattern string, fn handler.HandlerFunc[T, R], opts ...handler.HandleOption) {
	r.Handle(http.MethodPatch, pattern, handler.HandleRequest(fn, opts...))
}

// DELETE registers a typed DELETE action (no response body, 204 by
// default — override with handler.WithSuccessCode).
func DELETE[T any](r kernel.Router, pattern string, fn handler.ActionFunc[T], opts ...handler.HandleOption) {
	r.Handle(http.MethodDelete, pattern, handler.HandleAction(fn, opts...))
}
