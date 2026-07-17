package store

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #3 regression tests: error provenance is decided per entry
// point. Field-NAME errors (where.ErrUnknownField) on programmatic entry
// points are server bugs and pass through raw (→ 500, chain intact); the
// ListFromQuery chain keeps mapping them to 400 because there the names
// arrive in the URL. Value errors (where.ErrInvalidParam) map to 400
// everywhere — sizes and filter values legitimately flow from clients.

// assertServerFieldError asserts the raw ErrUnknownField shape: chain
// preserved, not pre-mapped to a client 400.
func assertServerFieldError(t *testing.T, err error, entry string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error for the unknown field", entry)
	}
	if !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("%s: must keep the ErrUnknownField chain, got %v", entry, err)
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("%s: a programmatic field typo must NOT surface as client input, got %v", entry, err)
	}
}

func TestErrorProvenance_ProgrammaticUnknownFieldIsServerError(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()
	u := createUser(t, s, "alice", "prov-alice@test")

	_, listErr := s.List(ctx, where.WithFilter("typo", "x"))
	assertServerFieldError(t, listErr, "List")

	_, countErr := s.Count(ctx, where.WithFilter("typo", "x"))
	assertServerFieldError(t, countErr, "Count")

	_, getErr := s.Get(ctx, Where(where.WithFilter("typo", "x")))
	assertServerFieldError(t, getErr, "Get(Where)")

	updErr := s.Update(ctx, Where(where.WithFilter("typo", "x")), Set(map[string]any{"name": "n"}))
	assertServerFieldError(t, updErr, "Update(Where)")

	delErr := s.Delete(ctx, Where(where.WithFilter("typo", "x")))
	assertServerFieldError(t, delErr, "Delete(Where)")

	_, exErr := s.Exists(ctx, Where(where.WithFilter("typo", "x")))
	assertServerFieldError(t, exErr, "Exists(Where)")

	_, inErr := ListIn(ctx, s, "typo", []string{"x"})
	assertServerFieldError(t, inErr, "ListIn")

	_, curErr := s.ListWithCursor(ctx, "typo", where.CursorAfter, "", 5)
	assertServerFieldError(t, curErr, "ListWithCursor field")

	// Sanity: the store still works for declared fields.
	if _, err := s.Get(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
}

func TestErrorProvenance_ValueErrorsStayClientMapped(t *testing.T) {
	// The split is field NAMES versus VALUES: bad values keep the 400
	// mapping on every entry point — handlers legitimately pass client
	// pagination values straight into WithPage / WithLimit / cursor size.
	s, _ := setupUserStore(t)
	ctx := context.Background()

	if _, err := s.List(ctx, where.WithPage(0, 10)); !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("bad page value must stay ErrInvalidArgument, got %v", err)
	}
	if _, err := s.List(ctx, where.WithLimit(where.MaxPageSize+1)); !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("oversized limit must stay ErrInvalidArgument, got %v", err)
	}
	if _, err := s.ListWithCursor(ctx, "name", where.CursorAfter, "!!not-a-token!!", 5); !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("malformed cursor token must stay ErrInvalidArgument, got %v", err)
	}
}

func TestErrorProvenance_ListFromQueryChainStays400(t *testing.T) {
	// The one client entry point: unknown field names arriving in the URL
	// keep the invalid-argument mapping — the order parameter exercises
	// the parse leg (parseOrder resolves fields eagerly).
	s, _ := setupUserStore(t)
	ctx := context.Background()

	_, err := s.ListFromQuery(ctx, url.Values{"order": {"typo:asc"}})
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("ListFromQuery unknown order field must map to 400, got %v", err)
	}

	// Unknown filter params: silently ignored when permissive (no error),
	// 400 under WithStrict — both unchanged.
	if _, err := s.ListFromQuery(ctx, url.Values{"typo": {"x"}}); err != nil {
		t.Fatalf("permissive ListFromQuery must ignore unknown params, got %v", err)
	}

	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	strict := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email"),
		WithUpdateFields("name", "email"),
		WithStrict(),
	)
	if _, err := strict.ListFromQuery(ctx, url.Values{"typo": {"x"}}); !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("strict ListFromQuery unknown param must map to 400, got %v", err)
	}
}

func TestErrorProvenance_MapperClassificationMatrix(t *testing.T) {
	// Unit pin of the two mappers' split, including the seam-guard
	// property ListFromQuery relies on: mapClientQueryError is idempotent
	// over already-mapped apierr values and maps a raw ErrUnknownField
	// that might one day leak out of its List leg.
	rawField := where.ErrUnknownField
	rawValue := where.ErrInvalidParam
	rawConfig := where.ErrFieldNotConfigured

	if got := mapQueryError(rawField); !errors.Is(got, where.ErrUnknownField) || errors.Is(got, apierr.ErrInvalidArgument) {
		t.Fatalf("programmatic mapper must pass field errors through raw, got %v", got)
	}
	if got := mapQueryError(rawValue); !errors.Is(got, apierr.ErrInvalidArgument) {
		t.Fatalf("programmatic mapper must map value errors to 400, got %v", got)
	}
	if got := mapQueryError(rawConfig); !errors.Is(got, where.ErrFieldNotConfigured) {
		t.Fatalf("config errors pass through, got %v", got)
	}

	if got := mapClientQueryError(rawField); !errors.Is(got, apierr.ErrInvalidArgument) {
		t.Fatalf("client mapper must map field errors to 400, got %v", got)
	}
	if got := mapClientQueryError(rawValue); !errors.Is(got, apierr.ErrInvalidArgument) {
		t.Fatalf("client mapper must map value errors to 400, got %v", got)
	}
	if got := mapClientQueryError(rawConfig); !errors.Is(got, where.ErrFieldNotConfigured) {
		t.Fatalf("client mapper passes config errors through, got %v", got)
	}
	mapped := apierr.ErrInvalidArgument.WithMessage("already mapped")
	if got := mapClientQueryError(mapped); got != error(mapped) {
		t.Fatalf("client mapper must be idempotent over mapped values, got %v", got)
	}
}
