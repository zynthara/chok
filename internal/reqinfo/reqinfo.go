// Package reqinfo carries the per-request facts that middleware outside
// the router needs once the handler has run — the matched route pattern,
// for access logs and telemetry — without widening chok's public API.
package reqinfo

import "context"

// Info is filled in as the request travels inward.
type Info struct {
	// Route is the matched path pattern, such as /api/v1/posts/{id};
	// empty when no route matched.
	Route string
}

type key struct{}

// New returns a context carrying a fresh Info.
func New(ctx context.Context) (context.Context, *Info) {
	info := &Info{}
	return context.WithValue(ctx, key{}, info), info
}

// From returns the request's Info, or nil outside a chok handler.
func From(ctx context.Context) *Info {
	info, _ := ctx.Value(key{}).(*Info)
	return info
}
