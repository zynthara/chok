package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store/where"
)

// TestErrors_APIError pins what each error answers and what it matches.
func TestErrors_APIError(t *testing.T) {
	sentinels := []*chok.Error{chok.ErrBadRequest, chok.ErrUnauthorized, chok.ErrNotFound, chok.ErrConflict, chok.ErrInternal}
	for _, c := range []struct {
		err    error
		code   int
		reason string
		meta   map[string]any
		is     *chok.Error
	}{
		{&NotFoundError{Locator: "rid x"}, 404, "NotFound", nil, chok.ErrNotFound},
		{&VersionConflictError{Locator: "rid x", Version: 3}, 409, "Conflict", map[string]any{"version": 3}, chok.ErrConflict},
		{&DuplicateError{Field: "title", Constraint: "uk"}, 409, "Conflict", map[string]any{"field": "title"}, chok.ErrConflict},
		{&DuplicateError{Constraint: "uk_raw"}, 409, "Conflict", map[string]any{"constraint": "uk_raw"}, chok.ErrConflict},
		{&DuplicateError{}, 409, "Conflict", nil, chok.ErrConflict},
		{&EmptyChangesError{}, 400, "BadRequest", nil, chok.ErrBadRequest},
		{&NoPrincipalError{}, 401, "Unauthorized", nil, chok.ErrUnauthorized},
		{&MissingConditionsError{}, 500, "InternalError", nil, nil},
	} {
		wrapped := fmt.Errorf("handler: %w", c.err)
		coder, ok := errors.AsType[interface {
			error
			chok.Coder
		}](wrapped)
		if !ok {
			t.Fatalf("%T is not a Coder through wrapping", c.err)
		}
		e := coder.APIError()
		if e.Code != c.code || e.Reason != c.reason || e.Message == "" {
			t.Errorf("%T: %d %s %q", c.err, e.Code, e.Reason, e.Message)
		}
		for k, v := range c.meta {
			if e.Metadata[k] != v {
				t.Errorf("%T: metadata %v, want %s=%v", c.err, e.Metadata, k, v)
			}
		}
		if len(c.meta) == 0 && len(e.Metadata) != 0 {
			t.Errorf("%T: unexpected metadata %v", c.err, e.Metadata)
		}
		for _, s := range sentinels {
			if got := errors.Is(wrapped, s); got != (s == c.is) {
				t.Errorf("%T: errors.Is(%s) = %v", c.err, s.Reason, got)
			}
		}
	}
	// A duplicate keeps the driver error it came from.
	cause := errors.New("driver says no")
	if !errors.Is(&DuplicateError{cause: cause}, cause) {
		t.Fatal("DuplicateError unwraps to its cause")
	}
}

// sqliteErr imitates the pure-Go SQLite driver's error: an extended
// result code and its message.
type sqliteErr struct {
	code int
	msg  string
}

func (e sqliteErr) Error() string { return e.msg }
func (e sqliteErr) Code() int     { return e.code }

func TestIsDuplicate(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("create: %w", &pgconn.PgError{Code: "23505", Message: "duplicate key value"}), true},
		{&pgconn.PgError{Code: "23503", Message: "violates foreign key constraint"}, false},
		{fmt.Errorf("x: %w", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'a' for key 't.uk'"}), true},
		{&mysql.MySQLError{Number: 1452, Message: "Cannot add or update a child row"}, false},
		{sqliteErr{1555, "UNIQUE constraint failed: t.id"}, true},
		{sqliteErr{2067, "UNIQUE constraint failed: t.code"}, true},
		{sqliteErr{2579, "UNIQUE constraint failed: t.rowid"}, true},
		// The code decides, whatever the text says.
		{sqliteErr{275, "CHECK constraint failed: UNIQUE constraint failed"}, false},
		{sqliteErr{787, "FOREIGN KEY constraint failed"}, false},
		{fmt.Errorf("wrapped: %w", gorm.ErrDuplicatedKey), true},
		// Untyped errors fall back to the message.
		{errors.New("Error 1062 (23000): Duplicate entry 'x' for key 'uk'"), true},
		{errors.New("UNIQUE constraint failed: posts.title"), true},
		{errors.New("connection refused"), false},
	} {
		if got := isDuplicate(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

func TestConstraintName_AllDialects(t *testing.T) {
	for msg, want := range map[string]string{
		"Error 1062 (23000): Duplicate entry 'a@b.com' for key 'uk_email'":                                    "uk_email",
		"Error 1062 (23000): Duplicate entry 'a@b.com-' for key 'users.uk_email'":                             "uk_email",
		`ERROR: duplicate key value violates unique constraint "uk_email" (SQLSTATE 23505)`:                   "uk_email",
		"constraint failed: UNIQUE constraint failed: users.email, users.delete_token (2067)":                 "users.email, users.delete_token (2067)",
		"UNIQUE constraint failed: users.email":                                                               "users.email",
		"something else entirely":                                                                             "",
		`ERROR: duplicate key value violates unique constraint "idx_posts_rid" DETAIL: Key (rid)=(x) exists.`: "idx_posts_rid",
	} {
		if got := constraintName(msg); got != want {
			t.Errorf("%q → %q, want %q", msg, got, want)
		}
	}
}

// fenced has a composite SoftUnique with the owner, a named unique index
// and a unique tag, so the field of every constraint shape is known.
type fenced struct {
	db.OwnedSoftDeleteModel
	Title string `json:"title" store:"query,update" gorm:"size:50;not null"`
	Code  string `json:"code" store:"query" gorm:"column:code_col;size:20;not null;uniqueIndex:uk_fenced_code"`
	Slug  string `json:"slug" store:"update" gorm:"size:20;not null;unique"`
	Note  string `json:"note" gorm:"size:20;not null;default:''"`
}

func fencedStore(t *testing.T, opts ...Option) *Store[fenced] {
	t.Helper()
	d := dbtest.Open(t, db.Tables(db.Table(&fenced{}, db.SoftUnique("uk_fenced_title", "title", "owner_id"))))
	return New[fenced](d, opts...)
}

// TestConstraintField_Resolution: a constraint name or SQLite's column
// list resolves to a field — the declared one first, else the first
// column that is not owner_id or delete_token, by its JSON name.
func TestConstraintField_Resolution(t *testing.T) {
	s := fencedStore(t)
	for name, want := range map[string]string{
		"uk_fenced_title":  "title",
		"uk_fenced_code":   "code",
		"uni_fenceds_slug": "slug",
		"idx_fenceds_r_id": "id", // gorm names it after the Go field RID
		"fenceds.owner_id, fenceds.title, fenceds.delete_token (2067)": "title",
		"fenceds.title, fenceds.owner_id, fenceds.delete_token":        "title",
		"fenceds.code_col":    "code",
		"fenceds.slug (2067)": "slug",
		"fenceds.note":        "note", // not a declared field: its JSON name
		"fenceds.mystery":     "mystery",
		"uk_unknown":          "",
	} {
		if got := s.constraintField(name); got != want {
			t.Errorf("%q → %q, want %q", name, got, want)
		}
	}
	declared := fencedStore(t, Constraints(map[string]string{"uk_fenced_title": "headline", "code_col": "the_code"}))
	if got := declared.constraintField("uk_fenced_title"); got != "headline" {
		t.Errorf("a declared constraint wins: %q", got)
	}
	if got := declared.constraintField("fenceds.code_col (2067)"); got != "the_code" {
		t.Errorf("a declared column list wins: %q", got)
	}
}

// TestMapError_DuplicateOnEveryDialect drives real duplicate keys through
// the lane's driver: each constraint shape names its field, and an index
// the model does not know keeps its name on dialects that report one.
func TestMapError_DuplicateOnEveryDialect(t *testing.T) {
	s := fencedStore(t)
	ctx := chok.WithPrincipal(t.Context(), chok.Principal{UserID: "alice"})
	seed := &fenced{Title: "t", Code: "c", Slug: "s", Note: "n"}
	if err := s.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}
	for want, obj := range map[string]*fenced{
		"title": {Title: "t", Code: "c2", Slug: "s2"},
		"code":  {Title: "t3", Code: "c", Slug: "s3"},
		"slug":  {Title: "t4", Code: "c4", Slug: "s"},
	} {
		var dup *DuplicateError
		if err := s.Create(ctx, obj); !errors.As(err, &dup) || dup.Field != want {
			t.Errorf("duplicate %s: %#v", want, err)
		}
	}
	// Another owner may reuse the title: owner_id is part of the index.
	if err := s.Create(chok.WithPrincipal(t.Context(), chok.Principal{UserID: "bob"}), &fenced{Title: "t", Code: "b", Slug: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.d.Unsafe(ctx).Exec("CREATE UNIQUE INDEX ux_fenced_note ON fenceds (note)").Error; err != nil {
		t.Fatal(err)
	}
	err := s.Create(ctx, &fenced{Title: "t5", Code: "c5", Slug: "s5", Note: "n"})
	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("raw index: %v", err)
	}
	if dbtest.Driver() == "sqlite" {
		// SQLite names the columns, which resolve to the field.
		if dup.Field != "note" {
			t.Fatalf("sqlite raw index: %+v", dup)
		}
	} else if dup.Field != "" || dup.Constraint != "ux_fenced_note" || dup.APIError().Metadata["constraint"] != "ux_fenced_note" {
		t.Fatalf("an index the store does not know keeps its name: %+v %v", dup, dup.APIError())
	}
}

// TestQueryError_Classification: a malformed query option is the client's
// 400, whichever entry point it came through; anything else passes
// through untouched.
func TestQueryError_Classification(t *testing.T) {
	for _, base := range []error{where.ErrInvalidParam, where.ErrUnknownField} {
		in := fmt.Errorf("%w: page 0", base)
		out := queryError(in)
		e, ok := errors.AsType[*chok.Error](out)
		if !ok || e.Code != 400 || e.Reason != "BadRequest" || strings.HasPrefix(e.Message, "where:") || !errors.Is(out, base) {
			t.Errorf("%v → %#v", in, out)
		}
	}
	other := errors.New("boom")
	if queryError(other) != other || queryError(nil) != nil {
		t.Fatal("other errors pass through")
	}
}
