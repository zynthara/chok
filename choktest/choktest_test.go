package choktest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/apptest"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

type item struct {
	db.OwnedModel
	Name string `json:"name" store:"query,update" gorm:"size:50;not null"`
}

func (item) RIDPrefix() string { return "itm" }

// TestDB_OneDatabasePerTest: each call opens the lane's dialect on a
// database of its own, migrated, and drops it when the test ends.
func TestDB_OneDatabasePerTest(t *testing.T) {
	d := choktest.DB(t, db.Table(&item{}))
	ctx := t.Context()
	if got, want := d.Unsafe(ctx).Name(), dbtest.Driver(); got != want {
		t.Fatalf("dialect %s, lane %s", got, want)
	}
	if err := d.Unsafe(ctx).Create(&item{Name: "x", OwnerID: "u"}).Error; err != nil {
		t.Fatal(err)
	}
	var where string
	t.Run("another", func(t *testing.T) {
		other := choktest.DB(t, db.Table(&item{}))
		var n int64
		if err := other.Unsafe(ctx).Model(&item{}).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("a second database sees %d rows, %v", n, err)
		}
		where = location(t, other)
		if !exists(t, d, where) {
			t.Fatalf("%s does not exist while the test runs", where)
		}
	})
	if exists(t, d, where) {
		t.Fatalf("%s survives its test", where)
	}
}

// location names where a handle's data lives: the SQLite file, the
// Postgres schema or the MySQL database.
func location(t *testing.T, d *db.DB) string {
	t.Helper()
	gdb := d.Unsafe(t.Context())
	var name string
	var err error
	switch dbtest.Driver() {
	case "sqlite":
		var seq int
		var schema string
		err = gdb.Raw("PRAGMA database_list").Row().Scan(&seq, &schema, &name)
	case "postgres":
		err = gdb.Raw("SELECT current_schema()").Row().Scan(&name)
	case "mysql":
		err = gdb.Raw("SELECT DATABASE()").Row().Scan(&name)
	}
	if err != nil || name == "" {
		t.Fatalf("location: %q %v", name, err)
	}
	return name
}

func exists(t *testing.T, via *db.DB, name string) bool {
	t.Helper()
	if dbtest.Driver() == "sqlite" {
		_, err := os.Stat(name)
		return err == nil
	}
	var n int64
	if err := via.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Row().Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestStore_ReadyToUse(t *testing.T) {
	items := choktest.Store[item](t)
	ctx := choktest.Principal(t.Context(), chok.Principal{UserID: "alice"})
	it := &item{Name: "first"}
	if err := items.Create(ctx, it); err != nil {
		t.Fatal(err)
	}
	got, err := items.Get(ctx, store.RID(it.RID))
	if err != nil || got.Name != "first" || got.OwnerID != "alice" {
		t.Fatalf("got %+v %v", got, err)
	}
	// Principal is what the owner scope reads.
	bob := choktest.Principal(t.Context(), chok.Principal{UserID: "bob"})
	if n, err := items.Count(bob); err != nil || n != 0 {
		t.Fatalf("bob counts %d, %v", n, err)
	}
	if p, ok := chok.PrincipalFrom(bob); !ok || p.UserID != "bob" {
		t.Fatalf("principal %+v", p)
	}
}

// component records its lifecycle.
type component struct {
	started, healthy, closed atomic.Bool
	checks                   atomic.Int32
}

func (c *component) Start(ctx context.Context) error {
	c.started.Store(true)
	<-ctx.Done()
	return nil
}

// Health fails twice, so the start phase has to wait for it.
func (c *component) Health(context.Context) error {
	if c.checks.Add(1) < 3 {
		return errors.New("warming up")
	}
	c.healthy.Store(true)
	return nil
}

func (c *component) Close(context.Context) error {
	c.closed.Store(true)
	return nil
}

type hello struct {
	Name string `query:"name" validate:"required"`
}

// TestApp_LifecycleInProcess: App returns once the start phase is over,
// serves without a port, and stops at cleanup — skipping the drain
// delay, which here is longer than the test may take.
func TestApp_LifecycleInProcess(t *testing.T) {
	comp := &component{}
	var app *chok.App
	var base chok.Base
	begin := time.Now()
	t.Run("serve", func(t *testing.T) {
		base = chok.MustLoad[chok.Base]("choktest", chok.Args(nil))
		base.HTTP.DrainDelay = time.Minute
		c := choktest.App(t, base, func(a *chok.App) {
			app = a
			a.Use(comp)
			chok.GET(a.Router(), "/hello", func(_ context.Context, req *hello) (map[string]string, error) {
				return map[string]string{"hello": req.Name}, nil
			})
		})
		if !comp.started.Load() || !comp.healthy.Load() {
			t.Fatal("App returned before the start phase ended")
		}
		if app.Addr() != "" {
			t.Fatalf("listening on %s", app.Addr())
		}
		if code := c.GetJSON("/readyz", nil); code != http.StatusOK {
			t.Fatalf("readyz %d", code)
		}
		var out map[string]string
		if code := c.GetJSON("/hello?name=chok", &out); code != http.StatusOK || out["hello"] != "chok" {
			t.Fatalf("hello = %d %v", code, out)
		}
	})
	if !comp.closed.Load() {
		t.Fatal("the stop phase did not close the component")
	}
	if took := time.Since(begin); took > 30*time.Second {
		t.Fatalf("the stop phase waited out the drain delay: %v", took)
	}
	if !base.HTTP.Enabled {
		t.Fatal("the caller's Base was changed")
	}
}

// TestApp_ZeroBaseIsTheDefaults: a zero Base serves the default
// endpoints, so a test needs no configuration at all, and keeps the
// default CSRF protection on.
func TestApp_ZeroBaseIsTheDefaults(t *testing.T) {
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		chok.POST(app.Router(), "/x", func(context.Context, *chok.None) (chok.None, error) { return chok.None{}, nil })
	})
	for _, path := range []string{"/healthz", "/livez", "/readyz", "/openapi.json", "/docs"} {
		resp := c.Do(httptest.NewRequest(http.MethodGet, path, nil))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", path, resp.StatusCode)
		}
	}
	if code := c.PostJSON("/x", nil, nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("a cross-site POST = %d; csrf is on by default", code)
	}
	if code := c.PostJSON("/x", nil, nil); code != http.StatusNoContent {
		t.Errorf("a POST without the browser's headers = %d", code)
	}
}

type note struct {
	Text  string        `json:"text" validate:"required"`
	Every time.Duration `json:"every"`
}

type echo struct {
	Text          string        `json:"text"`
	Every         time.Duration `json:"every"`
	Authorization string        `json:"authorization"`
	Trace         string        `json:"trace"`
}

func TestClient_JSON(t *testing.T) {
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		r := app.Router()
		chok.POST(r, "/echo", func(ctx context.Context, req *struct {
			note
			Authorization string `header:"Authorization"`
			Trace         string `header:"X-Trace"`
		}) (*echo, error) {
			return &echo{req.Text, req.Every, req.Authorization, req.Trace}, nil
		}, chok.Created())
		chok.DELETE(r, "/things/{id}", func(context.Context, *struct {
			ID string `path:"id"`
		}) (chok.None, error) {
			return chok.None{}, nil
		})
	})
	var out echo
	if code := c.PostJSON("/echo", note{Text: "hi", Every: 90 * time.Minute}, &out, "X-Trace", "t1"); code != http.StatusCreated {
		t.Fatalf("post %d", code)
	}
	if out.Text != "hi" || out.Every != 90*time.Minute || out.Trace != "t1" || out.Authorization != "" {
		t.Fatalf("echo %+v", out)
	}
	// An error answer decodes too: the envelope.
	var e struct {
		Code     int            `json:"code"`
		Reason   string         `json:"reason"`
		Metadata map[string]any `json:"metadata"`
	}
	if code := c.PostJSON("/echo", map[string]any{}, &e); code != http.StatusBadRequest || e.Reason != "Validation" {
		t.Fatalf("validation = %d %+v", code, e)
	}
	// WithBearer adds the header to every request; a request's own wins.
	authed := c.WithBearer("tok")
	out = echo{}
	authed.PostJSON("/echo", note{Text: "x"}, &out)
	if out.Authorization != "Bearer tok" {
		t.Fatalf("bearer %+v", out)
	}
	authed.PostJSON("/echo", note{Text: "x"}, &out, "Authorization", "Basic abc")
	if out.Authorization != "Basic abc" {
		t.Fatalf("own header %+v", out)
	}
	if c.PostJSON("/echo", note{Text: "x"}, &out); out.Authorization != "" {
		t.Fatal("WithBearer changed the original client")
	}
	// No body, no output.
	if code := c.JSON(http.MethodDelete, "/things/x", nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete %d", code)
	}
	if code := c.GetJSON("/nowhere", nil); code != http.StatusNotFound {
		t.Fatalf("unknown route %d", code)
	}
	resp := c.Do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("Do: %d %v", resp.StatusCode, resp.Header)
	}
}

// TestStoreBehindApp: the pieces together, as an application's test
// uses them.
func TestStoreBehindApp(t *testing.T) {
	d := choktest.DB(t, db.Table(&item{}))
	items := store.New[item](d)
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		api := app.Group("/api", func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if id := r.Header.Get("X-User-Id"); id != "" {
					r = r.WithContext(chok.WithPrincipal(r.Context(), chok.Principal{UserID: id}))
				}
				next.ServeHTTP(w, r)
			})
		})
		chok.List(api, "/items", items)
	})
	ctx := choktest.Principal(t.Context(), chok.Principal{UserID: "alice"})
	for _, name := range []string{"a", "b"} {
		if err := items.Create(ctx, &item{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	var page chok.Page[item]
	if code := c.GetJSON("/api/items?order=name:desc", &page, "X-User-Id", "alice"); code != http.StatusOK || page.Total != 2 || page.Items[0].Name != "b" {
		t.Fatalf("alice's list = %d %+v", code, page)
	}
	page = chok.Page[item]{}
	if code := c.GetJSON("/api/items", &page, "X-User-Id", "bob"); code != http.StatusOK || page.Total != 0 || page.Items == nil {
		t.Fatalf("bob's list = %d %+v", code, page)
	}
	if n, _ := items.Count(ctx, where.Eq("name", "a")); n != 1 {
		t.Fatal(n)
	}
}

func TestPackage_LowersThePasswordCost(t *testing.T) {
	if got := apptest.PasswordCost.Load(); got != 4 { // bcrypt.MinCost
		t.Fatalf("a test binary importing choktest hashes at cost %d, want bcrypt's minimum, 4", got)
	}
}
