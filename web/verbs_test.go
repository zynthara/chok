package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/web"
)

type echoReq struct {
	Name string `json:"name"`
}

type echoResp struct {
	Greeting string `json:"greeting"`
}

// TestVerbs_RouteBindAndDocument: each helper is exactly
// r.Handle + the typed binding layer — method routing, body binding,
// success codes and error mapping all behave as if written longhand.
func TestVerbs_RouteBindAndDocument(t *testing.T) {
	sr := choktest.NewServeRouter()
	r, h := kernel.Router(sr), http.Handler(sr)

	web.GET(r, "/ping", func(context.Context, *struct{}) (string, error) {
		return "pong", nil
	})
	web.POST(r, "/echo", func(_ context.Context, req *echoReq) (*echoResp, error) {
		return &echoResp{Greeting: "hi " + req.Name}, nil
	}, handler.WithSuccessCode(http.StatusCreated))
	web.PUT(r, "/boom", func(context.Context, *struct{}) (string, error) {
		return "", apierr.ErrConflict.WithMessage("stale")
	})
	web.PATCH(r, "/touch", func(context.Context, *struct{}) (string, error) {
		return "touched", nil
	})
	web.DELETE(r, "/gone", func(context.Context, *struct{}) error {
		return nil
	})

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// GET with an empty request type — the hello-world shape.
	if w := do(http.MethodGet, "/ping", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("GET /ping = %d %s", w.Code, w.Body.String())
	}
	// Wrong method must not match the verb's registration.
	if w := do(http.MethodPost, "/ping", ""); w.Code == 200 {
		t.Fatalf("POST /ping must not hit the GET route, got %d", w.Code)
	}
	// POST body binding + success-code passthrough.
	if w := do(http.MethodPost, "/echo", `{"name":"go"}`); w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "hi go") {
		t.Fatalf("POST /echo = %d %s", w.Code, w.Body.String())
	}
	// Error mapping rides the same envelope as longhand handlers.
	if w := do(http.MethodPut, "/boom", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stale") {
		t.Fatalf("PUT /boom = %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPatch, "/touch", ""); w.Code != 200 {
		t.Fatalf("PATCH /touch = %d", w.Code)
	}
	// DELETE is an action: 204, empty body.
	if w := do(http.MethodDelete, "/gone", ""); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("DELETE /gone = %d %q, want 204 empty", w.Code, w.Body.String())
	}
}

// captureRouter records what Handle receives so the test can assert
// on the handler the helper actually registered.
type captureRouter struct {
	method, pattern string
	h               http.Handler
}

func (c *captureRouter) Handle(method, pattern string, h http.Handler, _ ...kernel.Middleware) {
	c.method, c.pattern, c.h = method, pattern, h
}
func (c *captureRouter) Group(string, ...kernel.Middleware) kernel.Router { return c }

// TestVerbs_MetaSurvivesForSwagger: the helpers keep the route-table
// metadata contract — a summary attached through a verb helper is
// visible to the documentation layer exactly like a longhand
// HandleRequest registration. Guards against a future helper
// bypassing the binding layer.
func TestVerbs_MetaSurvivesForSwagger(t *testing.T) {
	cr := &captureRouter{}
	web.GET(cr, "/documented", func(context.Context, *struct{}) (string, error) {
		return "ok", nil
	}, handler.WithSummary("Documented route"), handler.WithTags("verbs"))

	if cr.method != http.MethodGet || cr.pattern != "/documented" {
		t.Fatalf("registered %s %s", cr.method, cr.pattern)
	}
	m, ok := cr.h.(interface{ Meta() handler.Meta })
	if !ok {
		t.Fatal("verb-registered handler must carry Meta for the route table")
	}
	if m.Meta().Summary != "Documented route" {
		t.Fatalf("Meta.Summary = %q", m.Meta().Summary)
	}
}
