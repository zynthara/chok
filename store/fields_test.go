package store_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// account has a field only the URL filters on (name), one only Patch
// writes (code), one hidden from JSON (secret), an untagged one (amount)
// and the same struct embedded twice under two column prefixes.
type account struct {
	db.Model
	Name     string  `json:"name" store:"query" gorm:"size:50;not null"`
	Code     string  `json:"code" store:"update" gorm:"size:20;not null;uniqueIndex:uk_account_code"`
	Secret   string  `json:"-" gorm:"size:50;not null;uniqueIndex:uk_account_secret"`
	Amount   int64   `json:"amount" gorm:"not null;default:0"`
	Billing  address `gorm:"embedded;embeddedPrefix:billing_"`
	Shipping address `gorm:"embedded;embeddedPrefix:shipping_"`
}

// address is a named struct: its fields are not at the top level of the
// model's JSON, so the code names them by column.
type address struct {
	City string `json:"city" gorm:"size:50;not null;default:''"`
}

func (account) RIDPrefix() string { return "acc" }

func accounts(t *testing.T) *store.Store[account] {
	t.Helper()
	return store.New[account](dbtest.Open(t, db.Tables(db.Table(&account{}))))
}

// TestFields_CodeNamesEveryColumn: the store tags fence the URL, not the
// code — a json:"-" field, an update-only field, an untagged one and the
// managed columns all work in where, Order, the aggregates, Pluck,
// ListIn, Upsert's conflict and Set; Patch still writes only update
// fields.
func TestFields_CodeNamesEveryColumn(t *testing.T) {
	s := accounts(t)
	ctx := t.Context()
	if got := s.QueryFields(); !slices.Equal(got, []string{"created_at", "id", "name", "updated_at"}) {
		t.Fatalf("QueryFields = %v", got)
	}
	rows := []*account{
		{Name: "a", Code: "c1", Secret: "s1", Amount: 5, Billing: address{City: "Oslo"}},
		{Name: "b", Code: "c2", Secret: "s2", Amount: 7, Billing: address{City: "Oslo"}, Shipping: address{City: "Rome"}},
		{Name: "c", Code: "c3", Secret: "s3", Amount: 1, Billing: address{City: "Kyiv"}},
	}
	mustCreate(t, ctx, s, rows...)

	page, err := s.List(ctx, where.Gte("amount", 5), where.Order("secret", true))
	if err != nil || len(page.Items) != 2 || page.Items[0].Secret != "s2" {
		t.Fatalf("filter on untagged, order by json:\"-\": %+v %v", page, err)
	}
	if got, err := s.Get(ctx, store.Where(where.Eq("secret", "s1"), where.Eq("code", "c1"), where.Eq("version", 1))); err != nil || got.Name != "a" {
		t.Fatalf("get by json:\"-\", update-only and a managed column: %+v %v", got, err)
	}
	if n, err := s.Count(ctx, where.Eq("shipping_city", "Rome")); err != nil || n != 1 {
		t.Fatalf("a field of a named struct goes by its column: %d %v", n, err)
	}
	if sum, ok, err := s.Sum[int64](ctx, "amount", where.Eq("billing_city", "Oslo")); err != nil || !ok || sum != 12 {
		t.Fatalf("Sum: %d %v %v", sum, ok, err)
	}
	if n, err := s.CountDistinct(ctx, "billing_city"); err != nil || n != 2 {
		t.Fatalf("CountDistinct: %d %v", n, err)
	}
	groups, err := s.GroupBy[string](ctx, "billing_city", []store.Aggregate{{Func: "sum", Field: "amount"}})
	if err != nil || len(groups) != 2 || groups[0].Key != "Kyiv" {
		t.Fatalf("GroupBy: %+v %v", groups, err)
	}
	if secrets, err := s.Pluck[string](ctx, "secret", where.Order("amount")); err != nil || !slices.Equal(secrets, []string{"s3", "s1", "s2"}) {
		t.Fatalf("Pluck: %v %v", secrets, err)
	}
	if got, err := s.ListIn(ctx, "secret", []string{"s1", "s3", "nobody"}); err != nil || len(got) != 2 {
		t.Fatalf("ListIn: %+v %v", got, err)
	}

	// Upsert on a json:"-" key updates the row it collides with.
	if err := s.Upsert(ctx, &account{Name: "a2", Code: "c9", Secret: "s1"}, "secret"); err != nil {
		t.Fatalf("Upsert on secret: %v", err)
	}
	if got, err := s.Get(ctx, store.Where(where.Eq("secret", "s1"))); err != nil || got.Code != "c9" {
		t.Fatalf("upserted %+v %v", got, err)
	}
	if err := s.Upsert(ctx, &account{Name: "n", Code: "c2", Secret: "s7"}, "code"); err != nil {
		t.Fatalf("Upsert on an update-only key: %v", err)
	}
	if n, _ := s.Count(ctx); n != 3 {
		t.Fatalf("the upserts inserted rows: %d", n)
	}

	// Set writes any column but the managed ones.
	if err := s.Update(ctx, store.Where(where.Eq("secret", "s3")), store.Set(map[string]any{"amount": 9, "secret": "s4", "shipping_city": "Lima", "name": "z"})); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := s.Get(ctx, store.Where(where.Eq("secret", "s4"))); err != nil || got.Amount != 9 || got.Shipping.City != "Lima" || got.Name != "z" {
		t.Fatalf("after Set: %+v %v", got, err)
	}
	for _, field := range []string{"id", "version", "created_at", "updated_at"} {
		if err := s.Update(ctx, store.Where(where.Eq("secret", "s4")), store.Set(map[string]any{field: 1})); err == nil || !strings.Contains(err.Error(), "managed column") {
			t.Errorf("Set %s: %v", field, err)
		}
	}
	// Patch is the client's write: store:"update" fields only.
	req := &struct {
		Amount *int64 `json:"amount"`
	}{new(int64(3))}
	if err := s.Update(ctx, store.Where(where.Eq("secret", "s4")), store.Patch(req).Onto(nil)); err == nil || !strings.Contains(err.Error(), "not an update field") {
		t.Fatalf("Patch of an untagged field: %v", err)
	}
	if err := s.Delete(ctx, store.Where(where.Eq("billing_city", "Kyiv"))); err != nil {
		t.Fatalf("delete by a field of a named struct: %v", err)
	}
	if n, _ := s.Count(ctx); n != 2 {
		t.Fatalf("after delete: %d", n)
	}
}

// TestFields_UnknownFieldStatus: an unknown field in the URL is the
// client's 400 naming the allowed fields; in the code it is a 500 whose
// response names nothing and whose log names the model and the field,
// and errors.Is still matches where.ErrUnknownField.
func TestFields_UnknownFieldStatus(t *testing.T) {
	s := accounts(t)
	ctx := t.Context()
	var coder chok.Coder
	for name, err := range map[string]error{
		"where":         errOf(s.List(ctx, where.Eq("nope", 1))),
		"order":         errOf(s.List(ctx, where.Order("nope"))),
		"locator":       s.Delete(ctx, store.Where(where.Eq("nope", 1))),
		"aggregate":     second(s.Sum[int64](ctx, "nope")),
		"pluck":         errOf(s.Pluck[string](ctx, "nope")),
		"list in":       errOf(s.ListIn(ctx, "nope", []int{1})),
		"upsert":        s.Upsert(ctx, &account{Secret: "x"}, "nope"),
		"set":           s.Update(ctx, store.RID("acc_x"), store.Set(map[string]any{"nope": 1})),
		"numeric key":   errOf(s.List(ctx, where.Eq("ID", 1))),
		"go field name": errOf(s.List(ctx, where.Eq("Secret", "s1"))),
	} {
		if !errors.Is(err, where.ErrUnknownField) || errors.As(err, &coder) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ListFromQuery(ctx, mustQuery(t, "secret=s1")); !errors.Is(err, chok.ErrBadRequest) || !strings.Contains(err.Error(), "filter on one of created_at, id, name, updated_at") {
		t.Fatalf("a URL filter on a column without store:\"query\": %v", err)
	}

	var logs bytes.Buffer
	base, err := chok.Load[chok.Base]("storetest", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	app := chok.New(base, chok.Logger(slog.New(slog.NewTextHandler(&logs, nil))))
	chok.GET(app.Router(), "/accounts", func(ctx context.Context, _ *chok.None) (*chok.Page[account], error) {
		return s.List(ctx, where.Eq("secrte", "s1")) // the typo a 500 is for
	})
	chok.List(app.Router(), "/listed", s)
	h := app.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secrte") || strings.Contains(rec.Body.String(), "account") {
		t.Fatalf("code's unknown field: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), `store: account: where: unknown field \"secrte\"`) {
		t.Fatalf("the log names the model and the field: %s", logs.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/listed?secret=s1", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "filter on one of") {
		t.Fatalf("URL's unknown field: %d %s", rec.Code, rec.Body)
	}
}

// TestFields_PublicNameConflictPanics: two fields under one public name
// with different columns are refused at New; the numeric key and the RID
// both answering to id are not a conflict — id is the RID.
func TestFields_PublicNameConflictPanics(t *testing.T) {
	type clash struct {
		db.Model
		Title string `json:"title" store:"query" gorm:"column:heading;size:50"`
		Raw   string `json:"-" gorm:"column:title;size:50"` // its public name is its column
	}
	d := dbtest.Open(t, db.Tables(db.Table(&clash{})))
	mustPanic(t, `field name "title" maps to columns "heading" and "title"`, func() { store.New[clash](d) })

	s := accounts(t)
	ctx := t.Context()
	l := &account{Name: "a", Code: "c", Secret: "s"}
	mustCreate(t, ctx, s, l)
	if got, err := s.Get(ctx, store.Where(where.Eq("id", l.RID))); err != nil || got.ID != l.ID {
		t.Fatalf("id is the RID: %+v %v", got, err)
	}
}
