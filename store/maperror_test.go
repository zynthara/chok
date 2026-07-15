package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
