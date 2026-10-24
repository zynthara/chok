package store_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store"
)

type postID struct {
	ID string `path:"id"`
}

type editPost struct {
	ID      string  `path:"id"`
	Version *int    `json:"version"`
	Title   *string `json:"title"`
}

type newPost struct {
	Title string `json:"title"`
}

type envelope struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Metadata  map[string]any `json:"metadata"`
}

// TestErrors_HTTPEnvelope: store errors reach the client as chok
// envelopes with no mapping registered anywhere — a missing row is a
// 404, a conflict a 409 naming the field or version, an empty change a
// 400, an anonymous owned write a 401 and a programming error a 500.
func TestErrors_HTTPEnvelope(t *testing.T) {
	d := openDB(t)
	posts, docs := store.New[post](d), store.New[doc](d)
	base, err := chok.Load[chok.Base]("storetest", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	app := chok.New(base, chok.Logger(slog.New(slog.DiscardHandler)))
	r := app.Router()
	chok.GET(r, "/posts/{id}", func(ctx context.Context, req *postID) (*post, error) {
		return posts.Get(ctx, store.RID(req.ID))
	})
	chok.POST(r, "/posts", func(ctx context.Context, req *newPost) (*post, error) {
		p := &post{Title: req.Title}
		return p, posts.Create(ctx, p)
	}, chok.Created())
	chok.PATCH(r, "/posts/{id}", func(ctx context.Context, req *editPost) (*post, error) {
		p, err := posts.Get(ctx, store.RID(req.ID))
		if err != nil {
			return nil, err
		}
		return p, posts.Update(ctx, store.RID(req.ID), store.Patch(req).Onto(p))
	})
	chok.List(r, "/posts", posts)
	chok.POST(r, "/docs", func(ctx context.Context, req *newPost) (*doc, error) {
		dd := &doc{Title: req.Title}
		return dd, docs.Create(ctx, dd)
	}, chok.Created())
	chok.DELETE(r, "/posts", func(ctx context.Context, _ *chok.None) (chok.None, error) {
		return chok.None{}, posts.Delete(ctx, store.Where())
	})
	h := app.Handler()
	call := func(method, path, body string) (int, envelope, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var e envelope
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return rec.Code, e, rec.Body.String()
	}

	code, _, body := call(http.MethodPost, "/posts", `{"title":"hello"}`)
	if code != http.StatusCreated {
		t.Fatalf("create %d %s", code, body)
	}
	var p post
	if err := json.Unmarshal([]byte(body), &p); err != nil || !strings.HasPrefix(p.RID, "pst_") {
		t.Fatalf("created %s %v", body, err)
	}
	for _, c := range []struct {
		method, path, body string
		code               int
		reason             string
		meta               map[string]any
	}{
		{"GET", "/posts/pst_missing", "", 404, "NotFound", nil},
		{"POST", "/posts", `{"title":"hello"}`, 409, "Conflict", map[string]any{"field": "title"}},
		{"PATCH", "/posts/" + p.RID, `{"version":7,"title":"x"}`, 409, "Conflict", map[string]any{"version": float64(7)}},
		{"PATCH", "/posts/" + p.RID, `{}`, 400, "BadRequest", nil},
		{"PATCH", "/posts/pst_missing", `{"title":"x"}`, 404, "NotFound", nil},
		{"GET", "/posts?nope=1", "", 400, "BadRequest", nil},
		{"GET", "/posts?order=title:sideways", "", 400, "BadRequest", nil},
		{"POST", "/docs", `{"title":"mine"}`, 401, "Unauthorized", nil},
		{"DELETE", "/posts", "", 500, "InternalError", nil},
	} {
		code, e, raw := call(c.method, c.path, c.body)
		if code != c.code || e.Code != c.code || e.Reason != c.reason || e.RequestID == "" {
			t.Errorf("%s %s = %d %s", c.method, c.path, code, raw)
			continue
		}
		for k, v := range c.meta {
			if e.Metadata[k] != v {
				t.Errorf("%s %s: metadata %v, want %s=%v", c.method, c.path, e.Metadata, k, v)
			}
		}
		if code == 500 && strings.Contains(raw, "store:") {
			t.Errorf("a 500 never carries the cause: %s", raw)
		}
	}
	if _, e, _ := call("GET", "/posts?nope=1", ""); !strings.Contains(e.Message, "filter on one of") {
		t.Errorf("the 400 lists the fields: %q", e.Message)
	}
}
