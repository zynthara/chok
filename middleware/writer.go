package middleware

import "net/http"

// The web server wraps every response in a written-tracking writer
// (web.ResponseWriter). Middleware read its state through these
// consumer-side assertions instead of importing web — the dependency
// arrow stays web → middleware (M2 mini-SPEC §1).

// statusOf returns the recorded response status: the tracker's value
// when present (200 for a handler that wrote a body without an
// explicit WriteHeader, per net/http semantics), 200 otherwise.
func statusOf(w http.ResponseWriter) int {
	if sw, ok := w.(interface{ Status() int }); ok {
		return sw.Status()
	}
	return http.StatusOK
}

// responseWritten reports whether a response already went out. Bare
// writers (no tracker) count as unwritten.
func responseWritten(w http.ResponseWriter) bool {
	ww, ok := w.(interface{ Written() bool })
	return ok && ww.Written()
}
