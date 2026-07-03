package kernel

import "net/http"

// Middleware is the framework-wide middleware shape: a plain
// http.Handler decorator. Defined in kernel (stdlib-only) so the
// mount contract does not depend on the web implementation package;
// web aliases these types for user-facing code (SPEC §3.2).
type Middleware = func(http.Handler) http.Handler

// Router is the route-registration contract handed to Mounter
// implementations and the user Routes callback during the mount
// phase. The kernel depends only on net/http here; the concrete
// implementation ships with the web package (M2). Tests assert
// mounting behaviour through doubles of this interface.
type Router interface {
	// Handle registers h for the method and pattern, wrapped by the
	// given middleware (outermost first).
	Handle(method, pattern string, h http.Handler, mw ...Middleware)

	// Group returns a sub-router rooted at prefix whose registrations
	// inherit the group middleware.
	Group(prefix string, mw ...Middleware) Router
}
