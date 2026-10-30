package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// post is the soft-delete fixture: a SoftUnique title, a query-only
// field, an update-only body and the scalar kinds aggregates read.
type post struct {
	db.SoftDeleteModel
	Title       string     `json:"title" store:"query,update" gorm:"size:200;not null"`
	Body        string     `json:"body" store:"update" gorm:"size:500;not null;default:''"`
	Status      string     `json:"status" store:"query,update" gorm:"size:20;not null;default:'draft'"`
	Views       int64      `json:"views" store:"query,update" gorm:"not null;default:0"`
	Score       float64    `json:"score" store:"query,update" gorm:"not null;default:0"`
	Hidden      bool       `json:"hidden" store:"query,update" gorm:"not null;default:false"`
	PublishedAt *time.Time `json:"published_at" store:"query,update"`
	Internal    string     `json:"internal" gorm:"size:20;not null;default:''"`
}

func (post) RIDPrefix() string { return "pst" }

// note is the hard-delete fixture with a plain unique index.
type note struct {
	db.Model
	Code string `json:"code" store:"query,update" gorm:"size:50;not null;uniqueIndex:uk_note_code"`
	N    int    `json:"n" store:"query,update" gorm:"not null;default:0"`
}

// doc is the owned fixture.
type doc struct {
	db.OwnedSoftDeleteModel
	Title string `json:"title" store:"query,update" gorm:"size:100;not null"`
}

func tables() db.Option {
	return db.Tables(
		db.Table(&post{}, db.SoftUnique("uk_post_title", "title")),
		db.Table(&note{}),
		db.Table(&doc{}, db.SoftUnique("uk_doc_title_owner", "title", "owner_id")),
	)
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t, tables())
}

func posts(t *testing.T, opts ...store.Option) *store.Store[post] {
	t.Helper()
	return store.New[post](openDB(t), opts...)
}

func as(ctx context.Context, user string, roles ...string) context.Context {
	return chok.WithPrincipal(ctx, chok.Principal{UserID: user, Roles: roles})
}

func mustCreate[T db.Modeler](t *testing.T, ctx context.Context, s *store.Store[T], objs ...*T) {
	t.Helper()
	for _, o := range objs {
		if err := s.Create(ctx, o); err != nil {
			t.Fatalf("create %+v: %v", o, err)
		}
	}
}

func titles(items []post) []string {
	out := make([]string, len(items))
	for i, p := range items {
		out[i] = p.Title
	}
	return out
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one containing %q", want)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, want) {
			t.Fatalf("panic %q; want it to contain %q", msg, want)
		}
	}()
	fn()
}

func TestNew_DeclaresFieldsFromTags(t *testing.T) {
	s := posts(t)
	want := []string{"created_at", "hidden", "id", "published_at", "score", "status", "title", "updated_at", "views"}
	if got := s.QueryFields(); !slices.Equal(got, want) {
		t.Fatalf("QueryFields = %v, want %v", got, want)
	}
	ctx := t.Context()
	p := &post{Title: "a"}
	mustCreate(t, ctx, s, p, &post{Title: "b"})
	// The public id is the RID: filters on "id" read the rid column.
	if page, err := s.List(ctx, where.Eq("id", p.RID)); err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
		t.Fatalf("filter on id: %+v %v", page, err)
	}
	// The tags fence the URL: an update-only or untagged field is not a
	// filter or an order there, a 400.
	for _, query := range []string{"body=x", "order=body", "internal=x"} {
		if _, err := s.ListFromQuery(ctx, mustQuery(t, query)); !errors.Is(err, chok.ErrBadRequest) || !errors.Is(err, where.ErrUnknownField) {
			t.Errorf("?%s: %v", query, err)
		}
	}
	// The code is not fenced: it filters and orders on any column.
	for name, opt := range map[string]where.Option{
		"filter on update-only": where.Eq("body", ""),
		"order by update-only":  where.Order("body"),
		"filter on untagged":    where.Eq("internal", ""),
	} {
		if page, err := s.List(ctx, opt); err != nil || len(page.Items) != 2 {
			t.Errorf("%s: %+v %v", name, page, err)
		}
	}
	// Set writes any column but the managed ones.
	for _, field := range []string{"created_at", "id", "version"} {
		if err := s.Update(ctx, store.Where(where.Eq("title", "a")), store.Set(map[string]any{field: "x"})); err == nil || !strings.Contains(err.Error(), "managed column") {
			t.Errorf("set %s: %v", field, err)
		}
	}
	if err := s.Update(ctx, store.Where(where.Eq("title", "a")), store.Set(map[string]any{"body": "b", "internal": "x"})); err != nil {
		t.Fatalf("set an update field and an untagged one: %v", err)
	}
	if got, _ := s.Get(ctx, store.RID(p.RID)); got.Body != "b" || got.Internal != "x" {
		t.Fatalf("stored %+v", got)
	}
}

// updateOnly declares nothing queryable: the base fields still are.
type updateOnly struct {
	db.Model
	Note string `json:"note" store:"update" gorm:"size:50;not null;default:''"`
}

// acronym has no json tags: the public names are gorm's column names.
type acronym struct {
	db.Model
	HTTPStatus string `store:"query,update" gorm:"size:20;not null"`
	Owner      string `json:"-" store:"query" gorm:"column:owner_name;size:20;not null;default:''"`
}

func TestNew_PublicNames(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&updateOnly{}), db.Table(&acronym{})))
	ctx := t.Context()
	u := store.New[updateOnly](d)
	if got := u.QueryFields(); !slices.Equal(got, []string{"created_at", "id", "updated_at"}) {
		t.Fatalf("update-only model queries %v", got)
	}
	n := &updateOnly{Note: "x"}
	mustCreate(t, ctx, u, n)
	if err := u.Update(ctx, store.RID(n.RID), store.Set(map[string]any{"note": "y"})); err != nil {
		t.Fatal(err)
	}
	if page, err := u.List(ctx, where.Order("created_at"), where.Eq("id", n.RID)); err != nil || page.Items[0].Note != "y" {
		t.Fatalf("base fields filter and order: %+v %v", page, err)
	}
	a := store.New[acronym](d)
	if got := a.QueryFields(); !slices.Equal(got, []string{"created_at", "http_status", "id", "owner_name", "updated_at"}) {
		t.Fatalf("names without json tags: %v", got)
	}
	mustCreate(t, ctx, a, &acronym{HTTPStatus: "created"}, &acronym{HTTPStatus: "gone"})
	if page, err := a.List(ctx, where.Eq("http_status", "created")); err != nil || len(page.Items) != 1 {
		t.Fatalf("filter by the column name of an acronym: %+v %v", page, err)
	}
}

func TestNew_RefusesDeclarationMistakes(t *testing.T) {
	d := openDB(t)
	type untagged struct {
		db.Model
		Name string `json:"name"`
	}
	mustPanic(t, "store:\"query,update\"", func() { store.New[untagged](d) })
	type badTag struct {
		db.Model
		Name string `json:"name" store:"filter"`
	}
	mustPanic(t, "the values are query, update", func() { store.New[badTag](d) })
	mustPanic(t, "T must be a struct", func() { store.New[*post](d) })
	mustPanic(t, `declares no field "nope"`, func() { store.New[post](d, store.Alias("nope", "x")) })
	mustPanic(t, "not a plain column", func() { store.New[post](d, store.Alias("title", "title; DROP TABLE posts")) })
	type managed struct {
		db.Model
		Owner string `json:"owner" gorm:"column:owner_id" store:"update"`
	}
	mustPanic(t, "managed column", func() { store.New[managed](d) })
	// No alias reopens a managed column either.
	for _, col := range []string{"version", "rid", "deleted_at", "delete_token", "created_at"} {
		mustPanic(t, "managed column", func() { store.New[post](d, store.Alias("body", col)) })
	}
	mustPanic(t, "given to the store of post", func() {
		store.New[post](d, store.Hooks(store.HookSet[note]{}))
	})
	mustPanic(t, "nil *db.DB", func() { store.New[post](nil) })
	mustPanic(t, "nil ScopeFunc", func() { store.Scope(nil) })
	mustPanic(t, "admin role 1 is blank", func() {
		store.New[doc](d, store.Policy(db.StorePolicy{AdminRoles: []string{"ops", " "}}))
	})
	for _, bad := range []map[string]string{{"": "title"}, {"uk_post_title": " "}} {
		mustPanic(t, "name a constraint and a field", func() { store.New[post](d, store.Constraints(bad)) })
	}
}

func TestNew_AliasMapsAColumn(t *testing.T) {
	type aliased struct {
		db.Model
		Label string `json:"label" store:"query,update" gorm:"column:name;size:50;not null"`
	}
	d := dbtest.Open(t, db.Tables(db.Table(&aliased{})))
	s := store.New[aliased](d, store.Alias("label", "name"))
	ctx := t.Context()
	mustCreate(t, ctx, s, &aliased{Label: "x"}, &aliased{Label: "y"})
	page, err := s.List(ctx, where.Eq("label", "y"))
	if err != nil || len(page.Items) != 1 || page.Items[0].Label != "y" {
		t.Fatalf("list by alias: %+v %v", page, err)
	}
	// An alias may qualify the column with the store's table.
	q := store.New[aliased](d, store.Alias("label", "aliaseds.name"))
	if n, err := q.Count(ctx, where.Eq("label", "x")); err != nil || n != 1 {
		t.Fatalf("qualified alias: %d %v", n, err)
	}
}

// UserBase lets indirectUser reach SoftDeleteModel through an embed;
// gorm maps only exported embedded structs, hence the exported name.
type UserBase struct {
	db.SoftDeleteModel
}

type indirectUser struct {
	UserBase
	Email string `json:"email" store:"query,update" gorm:"size:100;not null"`
}

func TestStore_IndirectSoftDelete(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&indirectUser{}, db.SoftUnique("uk_iu_email", "email"))))
	s := store.New[indirectUser](d)
	ctx := t.Context()
	u := &indirectUser{Email: "a@x"}
	mustCreate(t, ctx, s, u)
	if err := s.Delete(ctx, store.RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("the soft delete hides the row: %d", n)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 1 {
		t.Fatalf("and keeps it: %d", n)
	}
	mustCreate(t, ctx, s, &indirectUser{Email: "a@x"}) // the SoftUnique slot is free
	if err := s.Restore(ctx, store.RID(u.RID)); !errors.As(err, new(*store.DuplicateError)) {
		t.Fatalf("restore into a taken slot: %v", err)
	}
}

func TestTx_CommitsAndRollsBack(t *testing.T) {
	d := openDB(t)
	s, notes := store.New[post](d), store.New[note](d)
	ctx := t.Context()
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx context.Context) error {
		mustCreate(t, tx, s, &post{Title: "in tx"})
		mustCreate(t, tx, notes, &note{Code: "in tx"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("rolled back post persisted: %d", n)
	}
	if n, _ := notes.Count(ctx); n != 0 {
		t.Fatalf("a store on the same handle joins the transaction: %d notes", n)
	}
	if err := s.Tx(ctx, func(tx context.Context) error {
		mustCreate(t, tx, s, &post{Title: "kept"})
		// A read inside the transaction sees its writes.
		if n, err := s.Count(tx); err != nil || n != 1 {
			t.Errorf("count in tx = %d, %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, store.Where(where.Eq("title", "kept"))); err != nil || got.Title != "kept" {
		t.Fatalf("committed %+v %v", got, err)
	}
}

func TestUnsafe_IsScopedAndFailsClosed(t *testing.T) {
	d := openDB(t)
	docs := store.New[doc](d)
	alice, bob := as(t.Context(), "alice"), as(t.Context(), "bob")
	mustCreate(t, alice, docs, &doc{Title: "a1"}, &doc{Title: "a2"})
	mustCreate(t, bob, docs, &doc{Title: "b1"})
	var n int64
	if err := docs.Unsafe(alice).Count(&n).Error; err != nil || n != 2 {
		t.Fatalf("alice sees %d, %v", n, err)
	}
	if err := docs.Unsafe(t.Context()).Count(&n).Error; !errors.Is(err, chok.ErrUnauthorized) {
		t.Fatalf("no principal: %v", err)
	}
	// The escape hatch joins the transaction.
	_ = docs.Tx(alice, func(tx context.Context) error {
		mustCreate(t, tx, docs, &doc{Title: "a3"})
		if err := docs.Unsafe(tx).Count(&n).Error; err != nil || n != 3 {
			t.Errorf("Unsafe in tx sees %d, %v", n, err)
		}
		return nil
	})
	// A model without an owner needs no principal.
	notes := store.New[note](d)
	mustCreate(t, t.Context(), notes, &note{Code: "x"})
	if err := notes.Unsafe(t.Context()).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("unowned Unsafe: %d %v", n, err)
	}
}

// TestPolicy_InheritsAndIsCopied: a store starts from the handle's
// db.store, Policy replaces it whole, and neither can be changed behind
// the store's back afterwards.
func TestPolicy_InheritsAndIsCopied(t *testing.T) {
	roles := []string{"ops"}
	cfg := dbtest.Config(t)
	cfg.Store = db.StorePolicy{AdminRoles: roles, DefaultPageSize: 2, MaxPageSize: 3}
	d := dbtest.OpenConfig(t, cfg, tables())
	roles[0] = "mallory" // the handle copied its configuration
	ctx := t.Context()
	inherited := store.New[doc](d)
	for i := range 4 {
		mustCreate(t, as(ctx, "alice"), inherited, &doc{Title: fmt.Sprintf("d%d", i)})
	}
	page, err := inherited.List(as(ctx, "x", "ops"))
	if err != nil || page.Size != 2 || len(page.Items) != 2 {
		t.Fatalf("the handle's page size and admin role: %+v %v", page, err)
	}
	if page, _ = inherited.List(as(ctx, "x", "ops"), where.Page(1, 50)); page.Size != 3 {
		t.Fatalf("the handle's max: %+v", page)
	}
	if page, _ = inherited.List(as(ctx, "x", "mallory")); len(page.Items) != 0 {
		t.Fatalf("a role edited after Open is not an admin: %+v", page)
	}
	sp := d.StorePolicy()
	sp.AdminRoles[0] = "mallory"
	if page, _ = store.New[doc](d).List(as(ctx, "x", "mallory")); len(page.Items) != 0 {
		t.Fatal("StorePolicy() hands out a copy")
	}
	// Policy snapshots its argument when the option is made.
	mine := []string{"auditor"}
	opt := store.Policy(db.StorePolicy{AdminRoles: mine})
	mine[0] = "mallory"
	audited := store.New[doc](d, opt)
	if page, _ = audited.List(as(ctx, "x", "auditor")); len(page.Items) != 4 || page.Size != 20 {
		t.Fatalf("the policy as given, defaults for the sizes: %+v", page)
	}
	if page, _ = audited.List(as(ctx, "x", "mallory")); len(page.Items) != 0 {
		t.Fatal("a role edited after Policy() is not an admin")
	}
	if page, _ = audited.List(as(ctx, "x", "ops")); len(page.Items) != 0 {
		t.Fatal("Policy replaces the handle's roles")
	}
	// No policy pages past the 10,000 rows where accepts.
	huge := store.New[doc](d, store.Policy(db.StorePolicy{DefaultPageSize: 50_000, MaxPageSize: 50_000}))
	if page, err = huge.List(as(ctx, "alice")); err != nil || page.Size != 10_000 {
		t.Fatalf("the ceiling: %+v %v", page.Size, err)
	}
}

func storePolicy(def, maxSize int) db.StorePolicy {
	return db.StorePolicy{DefaultPageSize: def, MaxPageSize: maxSize}
}
