package ctxval

import "context"

// RoutePattern is a per-request slot for the matched route template.
//
// Go 1.22's ServeMux stamps r.Pattern on the innermost request copy it
// dispatches; outer middleware hold earlier shallow copies (each
// WithContext clones the struct) and never see it. The web server
// installs one RoutePattern before the middleware chain, the router
// fills it at dispatch, and post-next logic (access log, RED metrics,
// tracing span names) reads it — "unmatched" semantics fall out of the
// zero value.
//
// Writes happen strictly before reads on the request goroutine
// (handler returns, then deferred middleware logic runs), so no
// synchronization is needed.
type RoutePattern struct{ p string }

// Set records the matched pattern. Called by the router when a route
// dispatches; never called for unmatched requests.
func (rp *RoutePattern) Set(pattern string) {
	if rp != nil {
		rp.p = pattern
	}
}

// Get returns the recorded pattern, "" when none matched.
func (rp *RoutePattern) Get() string {
	if rp == nil {
		return ""
	}
	return rp.p
}

type routePatternKey struct{}

// WithRoutePattern installs a fresh RoutePattern slot and returns it
// alongside the derived context.
func WithRoutePattern(ctx context.Context) (context.Context, *RoutePattern) {
	rp := &RoutePattern{}
	return context.WithValue(ctx, routePatternKey{}, rp), rp
}

// RoutePatternHolder returns the installed slot, nil when absent
// (e.g. middleware exercised without the web server root handler).
func RoutePatternHolder(ctx context.Context) *RoutePattern {
	if ctx == nil {
		return nil
	}
	rp, _ := ctx.Value(routePatternKey{}).(*RoutePattern)
	return rp
}

// RoutePatternFrom returns the matched pattern for the request, "" for
// unmatched requests or when no slot was installed.
func RoutePatternFrom(ctx context.Context) string {
	return RoutePatternHolder(ctx).Get()
}
