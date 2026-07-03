package store

import (
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
