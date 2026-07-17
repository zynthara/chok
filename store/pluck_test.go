package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// --- two-table pair for the two-step IN showcase ---

type pluckSource struct {
	db.Model
	Name    string `json:"name" gorm:"size:50"`
	Enabled bool   `json:"enabled"`
}

func (pluckSource) RIDPrefix() string { return "psr" }

type pluckBook struct {
	db.Model
	SourceID uint   `json:"source_id"`
	Title    string `json:"title" gorm:"size:50"`
}

func (pluckBook) RIDPrefix() string { return "pbk" }

func TestPluck_OrderLimitAndValues(t *testing.T) {
	s := setupItemStore(t)
	ctx := context.Background()
	for _, code := range []string{"c", "b", "a"} {
		if err := s.Create(ctx, &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	codes, err := Pluck[string](ctx, s, "code", where.WithOrder("code"), where.WithLimit(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 2 || codes[0] != "a" || codes[1] != "b" {
		t.Fatalf("pluck = %v, want [a b]", codes)
	}
}

func TestPluck_EmptyResultIsNonNil(t *testing.T) {
	s := setupItemStore(t)
	got, err := Pluck[string](context.Background(), s, "code", where.WithFilter("code", "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("pluck must return a non-nil slice for zero matches")
	}
	if len(got) != 0 {
		t.Fatalf("pluck = %v, want empty", got)
	}
}

func TestPluck_UndeclaredFieldRejected(t *testing.T) {
	// Arch-backlog #3: Pluck field names are server code — an undeclared
	// one is a programming bug and keeps the raw where.ErrUnknownField
	// (→ 500), not a client-shaped 400.
	s := setupItemStore(t)
	ctx := context.Background()

	// A field that simply doesn't exist.
	if _, err := Pluck[string](ctx, s, "nope"); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("unknown field must surface as raw ErrUnknownField, got %v", err)
	}
	// A real column outside the query allowlist must be rejected too —
	// Pluck cannot become a side door around WithQueryFields.
	if _, err := Pluck[string](ctx, s, "rid"); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("undeclared column must surface as raw ErrUnknownField, got %v", err)
	}
}

func TestPluckDistinct_CollapsesDuplicates(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()
	for _, u := range []User{
		{Name: "bob", Email: "b1@example.com"},
		{Name: "bob", Email: "b2@example.com"},
		{Name: "ann", Email: "a1@example.com"},
	} {
		if err := s.Create(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}

	names, err := PluckDistinct[string](ctx, s, "name", where.WithOrder("name"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "ann" || names[1] != "bob" {
		t.Fatalf("distinct pluck = %v, want [ann bob]", names)
	}
}

func TestPluck_OwnerScopeIsolation(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&restoreDoc{})); err != nil {
		t.Fatal(err)
	}
	s := New[restoreDoc](gdb, log.Empty())

	alice, bob := userCtx("usr_alice"), userCtx("usr_bob")
	for _, d := range []struct {
		ctx   context.Context
		title string
	}{
		{alice, "draft"}, {alice, "final"}, {bob, "draft"},
	} {
		if err := s.Create(d.ctx, &restoreDoc{Title: d.title}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Pluck[string](alice, s, "title", where.WithOrder("title"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "draft" || got[1] != "final" {
		t.Fatalf("alice pluck = %v, want [draft final]", got)
	}
	if got, err := Pluck[string](bob, s, "title"); err != nil || len(got) != 1 {
		t.Fatalf("bob pluck = %v, %v; want exactly her one title", got, err)
	}
	// Fail-closed: no principal on ctx means no rows, not all rows.
	if _, err := Pluck[string](context.Background(), s, "title"); err == nil {
		t.Fatal("unauthenticated pluck on an owned model must fail closed")
	}
}

func TestPluck_ExcludesSoftDeleted(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()

	keep := &User{Name: "keep", Email: "keep@example.com"}
	gone := &User{Name: "gone", Email: "gone@example.com"}
	for _, u := range []*User{keep, gone} {
		if err := s.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, RID(gone.RID)); err != nil {
		t.Fatal(err)
	}

	names, err := Pluck[string](ctx, s, "name")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "keep" {
		t.Fatalf("pluck = %v, must exclude soft-deleted rows", names)
	}
}

func TestPluck_MaxPageSizeClamps(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	s := New[Item](gdb, log.Empty(), WithQueryFields("id", "code"), WithMaxPageSize(2))
	ctx := context.Background()
	for _, code := range []string{"a", "b", "c"} {
		if err := s.Create(ctx, &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Pluck[string](ctx, s, "code")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pluck returned %d values, per-store max page size must clamp to 2", len(got))
	}
}

func TestPluck_SeesRowsInsideTx(t *testing.T) {
	s := setupItemStore(t)
	ctx := context.Background()

	err := s.Tx(ctx, func(tx *Store[Item]) error {
		if err := tx.Create(ctx, &Item{Code: "staged"}); err != nil {
			return err
		}
		codes, err := Pluck[string](ctx, tx, "code")
		if err != nil {
			return err
		}
		if len(codes) != 1 || codes[0] != "staged" {
			t.Fatalf("pluck inside tx = %v, want the uncommitted row", codes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPluck_TwoStepIn pins the blessed cross-table pattern: pluck the
// key set from one store, feed it to WithFilterIn on the other. Both
// allowlists stay in force — no JOIN through Unsafe needed.
func TestPluck_TwoStepIn(t *testing.T) {
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&pluckSource{}), db.Table(&pluckBook{})); err != nil {
		t.Fatal(err)
	}
	sources := New[pluckSource](gdb, log.Empty(), WithQueryFields("id", "name", "enabled"))
	books := New[pluckBook](gdb, log.Empty(), WithQueryFields("id", "source_id", "title"))

	s1 := &pluckSource{Name: "s1", Enabled: true}
	s2 := &pluckSource{Name: "s2", Enabled: true}
	s3 := &pluckSource{Name: "s3", Enabled: false}
	for _, src := range []*pluckSource{s1, s2, s3} {
		if err := sources.Create(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*pluckBook{
		{SourceID: s1.ID, Title: "b1"},
		{SourceID: s2.ID, Title: "b2"},
		{SourceID: s3.ID, Title: "b3"},
		{SourceID: s1.ID, Title: "b4"},
	} {
		if err := books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := PluckIDs(ctx, sources, where.WithFilter("enabled", true))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("enabled source ids = %v, want 2", ids)
	}
	page, err := books.List(ctx, where.WithFilterIn("source_id", ids))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("books via enabled sources = %d, want 3", len(page.Items))
	}
}

// TestPluck_IDFieldYieldsRIDs pins the public-identifier discipline:
// the "id" field rides the standing id→rid alias, so plucking it
// returns RID strings — numeric keys stay behind PluckIDs.
func TestPluck_IDFieldYieldsRIDs(t *testing.T) {
	s := setupItemStore(t)
	ctx := context.Background()
	it := &Item{Code: "x"}
	if err := s.Create(ctx, it); err != nil {
		t.Fatal(err)
	}

	rids, err := Pluck[string](ctx, s, "id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rids) != 1 || rids[0] != it.RID {
		t.Fatalf("pluck id = %v, want the public RID %q", rids, it.RID)
	}
	ids, err := PluckIDs(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != it.ID {
		t.Fatalf("pluck ids = %v, want the internal key %d", ids, it.ID)
	}
}
