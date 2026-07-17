package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
)

// M3 acceptance (SPEC §5.3): the Postgres duplicate mapping. The
// dual-run store suite exercises it end-to-end against a live PG
// (TestCreate_DuplicateKey_ErrDuplicate on the postgres lane); these
// unit tests pin the typed tier and the constraint-name extraction
// against pgx's canonical error shapes.

func TestIsDuplicateError_PgconnTyped(t *testing.T) {
	dup := &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "uk_email"`}
	if !isDuplicateError(fmt.Errorf("create: %w", dup)) {
		t.Fatal("SQLSTATE 23505 must classify as duplicate through wrapping")
	}

	fk := &pgconn.PgError{Code: "23503", Message: `insert or update on table "posts" violates foreign key constraint "fk_author"`}
	if isDuplicateError(fk) {
		t.Fatal("a typed non-23505 PG error must NOT classify as duplicate — the code is authoritative")
	}
}

func TestIsDuplicateError_SQLiteConstraintCodesAreAuthoritative(t *testing.T) {
	h := setupDB(t)
	gdb := h.Unsafe(context.Background())
	if gdb.Dialector.Name() != "sqlite" {
		t.Skip("SQLite extended-code regression")
	}
	if err := gdb.Exec(`CREATE TABLE duplicate_probe (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		score INTEGER NOT NULL CHECK (score >= 0)
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("INSERT INTO duplicate_probe (id, email, score) VALUES (1, 'a@test', 1)").Error; err != nil {
		t.Fatal(err)
	}

	uniqueErr := gdb.Exec("INSERT INTO duplicate_probe (id, email, score) VALUES (2, 'a@test', 1)").Error
	if !isDuplicateError(uniqueErr) {
		t.Fatalf("SQLite UNIQUE violation must classify as duplicate: %v", uniqueErr)
	}
	for name, err := range map[string]error{
		"not null": gdb.Exec("INSERT INTO duplicate_probe (id, email, score) VALUES (3, NULL, 1)").Error,
		"check":    gdb.Exec("INSERT INTO duplicate_probe (id, email, score) VALUES (4, 'b@test', -1)").Error,
	} {
		if err == nil {
			t.Fatalf("%s probe unexpectedly succeeded", name)
		}
		if isDuplicateError(err) {
			t.Fatalf("SQLite %s violation misclassified as duplicate: %v", name, err)
		}
	}
	if err := gdb.Exec("CREATE TABLE duplicate_parent (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("CREATE TABLE duplicate_child (parent_id INTEGER REFERENCES duplicate_parent(id))").Error; err != nil {
		t.Fatal(err)
	}
	fkErr := gdb.Exec("INSERT INTO duplicate_child (parent_id) VALUES (999)").Error
	if fkErr == nil || isDuplicateError(fkErr) {
		t.Fatalf("SQLite foreign-key violation must remain a non-duplicate error: %v", fkErr)
	}
}

func TestMapError_PGDuplicateConstraintName(t *testing.T) {
	// The exact message text pgx surfaces for a unique violation.
	raw := errors.New(`ERROR: duplicate key value violates unique constraint "uk_email" (SQLSTATE 23505)`)
	mapped := mapError(raw)
	if !errors.Is(mapped, ErrDuplicate) {
		t.Fatalf("PG duplicate text must map to ErrDuplicate, got %v", mapped)
	}

	apiErr := MapError(mapped)
	if apiErr == nil {
		t.Fatal("MapError must translate ErrDuplicate")
	}
	if got := apiErr.Metadata["constraint"]; got != "uk_email" {
		t.Fatalf("constraint name extraction failed, got %q (metadata %v)", got, apiErr.Metadata)
	}
}

func TestExtractConstraintName_AllDialects(t *testing.T) {
	tests := []struct {
		detail string
		want   string
	}{
		{`Error 1062 (23000): Duplicate entry 'a@b.com' for key 'uk_email'`, "uk_email"},
		{`ERROR: duplicate key value violates unique constraint "uk_email" (SQLSTATE 23505)`, "uk_email"},
		{"UNIQUE constraint failed: users.email", "users.email"},
		{"something unrelated", ""},
	}
	for _, tt := range tests {
		if got := extractConstraintName(tt.detail); got != tt.want {
			t.Fatalf("extractConstraintName(%q) = %q, want %q", tt.detail, got, tt.want)
		}
	}
}

// --- Arch-backlog #14: declarative constraint→field mapping -------------
//
// Duplicate errors used to expose only the raw constraint/index name —
// schema naming that leaks layout and drifts with migrations. A Store can
// now declare WithConstraintFields; a declared hit reports the public
// field name instead, undeclared constraints keep the old behaviour.

func TestLookupConstraintField_DialectNormalization(t *testing.T) {
	fields := map[string]string{
		"uk_email":           "email",
		"email,delete_token": "email",
	}
	cases := []struct {
		name  string
		token string
		want  string
		ok    bool
	}{
		{"postgres bare name", "uk_email", "email", true},
		{"mysql 8 table-qualified key", "users.uk_email", "email", true},
		{"sqlite column list", "users.email, users.delete_token", "email", true},
		{"sqlite glebarez trailing code", "users.email, users.delete_token (2067)", "email", true},
		{"undeclared", "uk_something_else", "", false},
		{"undeclared qualified", "users.uk_other", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lookupConstraintField(fields, tc.token)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("lookup(%q) = (%q, %v), want (%q, %v)", tc.token, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// setupConstraintFieldStore builds a User store whose uk_email SoftUnique
// declaration covers all three dialect spellings: the index name (PG and
// MySQL report it) and the SoftUnique column list (SQLite reports columns
// only — delete_token included, since SoftUnique indexes carry it).
func setupConstraintFieldStore(t *testing.T, gdb *db.DB) *Store[User] {
	t.Helper()
	if err := gdb.Migrate(context.Background(), db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	return New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email"),
		WithUpdateFields("name", "email"),
		WithConstraintFields(map[string]string{
			"uk_email":           "email",
			"email,delete_token": "email",
		}),
	)
}

func TestConstraintFields_DuplicateMapsToField(t *testing.T) {
	s := setupConstraintFieldStore(t, setupDB(t))
	if err := s.Create(context.Background(), &User{Name: "a", Email: "dup@test"}); err != nil {
		t.Fatal(err)
	}
	err := s.Create(context.Background(), &User{Name: "b", Email: "dup@test"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	var dup *DuplicateEntryError
	if !errors.As(err, &dup) || dup.Field != "email" {
		t.Fatalf("declared constraint must resolve to the public field, got %+v", dup)
	}

	mapped := MapError(err)
	if mapped == nil {
		t.Fatal("MapError must map ErrDuplicate")
	}
	if got := mapped.Metadata["field"]; got != "email" {
		t.Fatalf("MapError must report metadata field=email, got %v (metadata %v)", got, mapped.Metadata)
	}
	if _, leaked := mapped.Metadata["constraint"]; leaked {
		t.Fatalf("a mapped duplicate must not also leak the constraint name: %v", mapped.Metadata)
	}
}

func TestConstraintFields_UndeclaredKeepsConstraintMetadata(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	// The declaration exists but names a DIFFERENT constraint — the
	// violated one is undeclared and must keep today's behaviour.
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email"),
		WithUpdateFields("name", "email"),
		WithConstraintFields(map[string]string{"uk_unrelated": "nickname"}),
	)
	if err := s.Create(context.Background(), &User{Name: "a", Email: "same@test"}); err != nil {
		t.Fatal(err)
	}
	err := s.Create(context.Background(), &User{Name: "b", Email: "same@test"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	var dup *DuplicateEntryError
	if !errors.As(err, &dup) || dup.Field != "" {
		t.Fatalf("undeclared constraint must not resolve a field, got %+v", dup)
	}
	mapped := MapError(err)
	if mapped == nil || mapped.Metadata["constraint"] == "" || mapped.Metadata["constraint"] == nil {
		t.Fatalf("undeclared constraint keeps the constraint metadata, got %v", mapped)
	}
	if _, present := mapped.Metadata["field"]; present {
		t.Fatalf("undeclared constraint must not invent a field, got %v", mapped.Metadata)
	}
}

func TestConstraintFields_EmptyDeclarationPanics(t *testing.T) {
	for name, decl := range map[string]map[string]string{
		"empty constraint": {"": "email"},
		"empty field":      {"uk_email": " "},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("empty declaration must panic at construction")
				}
			}()
			WithConstraintFields(decl)
		})
	}
}

// TestConstraintFields_MySQLDuplicateMapsToField pins the real MySQL 8
// message shape — keys reported as table.key — against the declaration
// normalization, on a live MySQL (make test-mysql lane).
func TestConstraintFields_MySQLDuplicateMapsToField(t *testing.T) {
	s := setupConstraintFieldStore(t, dbtest.OpenMySQL(t))
	if err := s.Create(context.Background(), &User{Name: "a", Email: "dup@mysql"}); err != nil {
		t.Fatal(err)
	}
	err := s.Create(context.Background(), &User{Name: "b", Email: "dup@mysql"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	var dup *DuplicateEntryError
	if !errors.As(err, &dup) || dup.Field != "email" {
		t.Fatalf("MySQL-reported key must normalise onto the declaration, got %+v (detail %q)", dup, dup.Detail)
	}
	mapped := MapError(err)
	if mapped == nil || mapped.Metadata["field"] != "email" {
		t.Fatalf("MapError must report metadata field=email, got %v", mapped)
	}
}
