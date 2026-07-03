package web

import "net/http"

// ResponseWriter is the written-tracking writer the server installs at
// the outermost layer of every request (SPEC §4.2 item 2). It backs
// the three "no double write" contracts — handler.WriteResponse,
// middleware.Recovery and middleware.Timeout all consult Written()
// (via structural assertion, so neither package imports web).
type ResponseWriter interface {
	http.ResponseWriter

	// Status returns the response code sent so far; 200 when the
	// handler wrote a body without an explicit WriteHeader, and also
	// 200 before anything was written (net/http's implicit default).
	Status() int

	// Written reports whether any part of the response (status line or
	// body bytes) has gone out.
	Written() bool

	// BytesWritten counts body bytes written so far.
	BytesWritten() int64

	// Unwrap exposes the underlying writer for http.ResponseController
	// (Flush / SetWriteDeadline / Hijack all resolve through it).
	Unwrap() http.ResponseWriter
}

// Wrap returns w as a ResponseWriter, wrapping once — writers that
// already track state pass through unchanged.
func Wrap(w http.ResponseWriter) ResponseWriter {
	if rw, ok := w.(ResponseWriter); ok {
		return rw
	}
	return &responseWriter{w: w}
}

type responseWriter struct {
	w      http.ResponseWriter
	status int
	wrote  bool
	bytes  int64
}

func (rw *responseWriter) Header() http.Header { return rw.w.Header() }

// WriteHeader forwards the first status line and swallows the rest —
// the guarded-write contract means duplicates are programming errors
// upstream, and forwarding them would only add net/http "superfluous
// WriteHeader" noise on top of the already-correct response.
func (rw *responseWriter) WriteHeader(code int) {
	if rw.wrote {
		return
	}
	rw.wrote = true
	rw.status = code
	rw.w.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wrote {
		rw.wrote = true
		rw.status = http.StatusOK
	}
	n, err := rw.w.Write(b)
	rw.bytes += int64(n)
	return n, err
}

func (rw *responseWriter) Status() int {
	if !rw.wrote {
		return http.StatusOK
	}
	return rw.status
}

func (rw *responseWriter) Written() bool      { return rw.wrote }
func (rw *responseWriter) BytesWritten() int64 { return rw.bytes }
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.w }

// Flush implements http.Flusher directly (code that type-asserts
// instead of using ResponseController). Flushing headers counts as
// writing.
func (rw *responseWriter) Flush() {
	if f, ok := rw.w.(http.Flusher); ok {
		if !rw.wrote {
			rw.wrote = true
			rw.status = http.StatusOK
		}
		f.Flush()
	}
}
