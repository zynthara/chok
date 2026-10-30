// Package choktest is what an application's tests need from chok: a
// throwaway database of the lane's dialect, a store on it, an App served
// in process, and a JSON client for that App.
//
//	func TestPosts(t *testing.T) {
//		c := choktest.App(t, chok.Base{}, func(app *chok.App) {
//			d := choktest.DB(t, db.Table(&Post{}))
//			chok.List(app.Router(), "/posts", store.New[Post](d))
//		})
//		var page chok.Page[Post]
//		if code := c.GetJSON("/posts", &page); code != http.StatusOK {
//			t.Fatal(code)
//		}
//	}
//
// The database follows CHOK_TEST_DRIVER: SQLite in a temporary file by
// default, or a database of its own on the server CHOK_TEST_PG_DSN or
// CHOK_TEST_MYSQL_DSN names, dropped when the test ends.
//
// A test binary that imports choktest hashes identity's passwords at
// bcrypt's minimum cost, so tests that register users stay fast;
// production binaries never import it.
package choktest

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/apptest"
	"github.com/zynthara/chok/internal/conf"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/internal/jsonx"
	"github.com/zynthara/chok/store"
)

// testPasswordCost is bcrypt.MinCost.
const testPasswordCost = 4

func init() { apptest.PasswordCost.Store(testPasswordCost) }

// DB opens a fresh database with the given tables, migrated, and closes
// and drops it when the test ends. It follows CHOK_TEST_DRIVER (sqlite,
// postgres or mysql); a server lane without its DSN fails the test.
func DB(t testing.TB, specs ...db.TableSpec) *db.DB {
	t.Helper()
	return dbtest.Open(t, db.Tables(specs...))
}

// Store returns the store of T on a fresh database holding T's table, as
// store.New would with opts. Declare SoftUnique indexes with DB and
// store.New instead.
func Store[T db.Modeler](t testing.TB, opts ...store.Option) *store.Store[T] {
	t.Helper()
	return store.New[T](DB(t, db.Table(new(T))), opts...)
}

// Principal returns ctx carrying p, for calling stores directly as an
// authenticated user would.
func Principal(ctx context.Context, p chok.Principal) context.Context {
	return chok.WithPrincipal(ctx, p)
}

// App builds an App from base — a zero Base means every default — runs
// setup on it to register components and routes, and runs its start
// phase: Starters start and every Healther must report healthy. The App
// serves in process through the returned Client and never listens on a
// port. When the test ends the stop phase runs, without the drain delay,
// and a failure to stop fails the test. The App logs warnings and errors
// to the test's output.
func App(t testing.TB, base chok.Base, setup func(app *chok.App)) *Client {
	t.Helper()
	if reflect.ValueOf(base).IsZero() {
		if err := conf.Defaults(&base); err != nil {
			t.Fatalf("choktest: defaults: %v", err)
		}
	}
	base.HTTP.Enabled = false // in process: the handler serves, nothing listens
	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn}))
	app := chok.New(base, chok.Logger(logger))
	if setup != nil {
		setup(app)
	}
	handler := app.Handler()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	var startErr error
	t.Cleanup(func() {
		cancel()
		if startErr != nil {
			return // Run has returned it already
		}
		if err := <-done; err != nil {
			t.Errorf("choktest: stop: %v", err)
		}
	})
	limit := base.App.StartTimeout
	if limit <= 0 {
		limit = 30 * time.Second
	}
	deadline := time.After(limit + 5*time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !apptest.Ready(app) {
		select {
		case startErr = <-done:
			if startErr == nil {
				startErr = errors.New("the App stopped before it was ready")
			}
			t.Fatalf("choktest: start: %v", startErr)
		case <-deadline:
			t.Fatalf("choktest: the App did not start within %s", limit)
		case <-tick.C:
		}
	}
	return &Client{t: t, h: handler}
}

// Client sends requests to an App in process. A request that cannot be
// encoded, or a response that does not decode into the value asked for,
// fails the test.
type Client struct {
	t      testing.TB
	h      http.Handler
	header http.Header
}

// Do serves req and returns the response, with its body read to the end
// and replayable. A relative URL is served as is.
func (c *Client) Do(req *http.Request) *http.Response {
	c.t.Helper()
	for k, vs := range c.header {
		if req.Header.Get(k) == "" {
			req.Header[k] = vs
		}
	}
	if req.RemoteAddr == "" {
		req.RemoteAddr = "192.0.2.1:1234"
	}
	if req.RequestURI == "" {
		req.RequestURI = req.URL.RequestURI()
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec.Result()
}

// GetJSON is JSON(http.MethodGet, path, nil, out, headers...).
func (c *Client) GetJSON(path string, out any, headers ...string) int {
	c.t.Helper()
	return c.JSON(http.MethodGet, path, nil, out, headers...)
}

// PostJSON is JSON(http.MethodPost, path, in, out, headers...).
func (c *Client) PostJSON(path string, in, out any, headers ...string) int {
	c.t.Helper()
	return c.JSON(http.MethodPost, path, in, out, headers...)
}

// JSON sends in as a JSON body (none when nil), decodes the response
// into out whatever its status — an error envelope too — unless out is
// nil or the body empty, and returns the status code. headers are
// key-value pairs: "X-Request-Id", "abc".
func (c *Client) JSON(method, path string, in, out any, headers ...string) int {
	c.t.Helper()
	if len(headers)%2 != 0 {
		c.t.Fatalf("choktest: %s %s: headers come in key-value pairs, got %d strings", method, path, len(headers))
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in, jsonx.Options)
		if err != nil {
			c.t.Fatalf("choktest: %s %s: encode the body: %v", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(c.t.Context(), method, path, body)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp := c.Do(req)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("choktest: %s %s: read the body: %v", method, path, err)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out, jsonx.Options); err != nil {
			c.t.Fatalf("choktest: %s %s: decode %d %s into %T: %v", method, path, resp.StatusCode, raw, out, err)
		}
	}
	return resp.StatusCode
}

// WithBearer returns a client that sends Authorization: Bearer token
// with every request, unless the request sets its own.
func (c *Client) WithBearer(token string) *Client {
	h := c.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Authorization", "Bearer "+token)
	return &Client{t: c.t, h: c.h, header: h}
}
