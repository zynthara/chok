package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseWriter_TracksExplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := Wrap(rec)

	if rw.Written() {
		t.Fatal("fresh writer must not report written")
	}
	if rw.Status() != 200 {
		t.Fatalf("implicit default status = %d, want 200", rw.Status())
	}

	rw.WriteHeader(404)
	if !rw.Written() || rw.Status() != 404 {
		t.Fatalf("after WriteHeader(404): written=%v status=%d", rw.Written(), rw.Status())
	}
	if rec.Code != 404 {
		t.Fatalf("status not forwarded: %d", rec.Code)
	}
}

func TestResponseWriter_ImplicitOKOnWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := Wrap(rec)

	n, err := rw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write: %d %v", n, err)
	}
	if !rw.Written() || rw.Status() != 200 {
		t.Fatalf("body-first write: written=%v status=%d", rw.Written(), rw.Status())
	}
	if rw.BytesWritten() != 5 {
		t.Fatalf("bytes = %d", rw.BytesWritten())
	}
}

func TestResponseWriter_SwallowsDuplicateWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := Wrap(rec)

	rw.WriteHeader(201)
	rw.WriteHeader(500) // guarded-write violation upstream — swallowed

	if rw.Status() != 201 || rec.Code != 201 {
		t.Fatalf("first status must win: tracker=%d recorder=%d", rw.Status(), rec.Code)
	}
}

func TestResponseWriter_WrapIsIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := Wrap(rec)
	if Wrap(rw) != rw {
		t.Fatal("Wrap must not re-wrap a tracking writer")
	}
}

func TestResponseWriter_UnwrapAndFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := Wrap(rec)

	if rw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap must expose the inner writer")
	}
	if f, ok := any(rw).(http.Flusher); ok {
		f.Flush()
		if !rw.Written() {
			t.Fatal("flush sends headers — must count as written")
		}
	} else {
		t.Fatal("tracking writer must implement http.Flusher")
	}
	if !rec.Flushed {
		t.Fatal("flush not forwarded")
	}
}
