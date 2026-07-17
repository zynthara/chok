package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #8 regression tests: ListIn is the auto-chunked second half
// of the two-step IN pattern — one IN's set semantics past where.MaxInList,
// under exactly List's allowlist/scope/soft-delete read path.

type ListInItem struct {
	db.SoftDeleteModel
	Code  string `json:"code" gorm:"size:32;not null"`
	Grade string `json:"grade" gorm:"size:8;not null;default:''"`
}

func (ListInItem) RIDPrefix() string { return "lii" }

func setupListInStore(t *testing.T, opts ...StoreOption) *Store[ListInItem] {
	t.Helper()
	return setupListInStoreOn(t, setupDB(t), opts...)
}

func setupListInStoreOn(t *testing.T, gdb *db.DB, opts ...StoreOption) *Store[ListInItem] {
	t.Helper()
	if err := gdb.Migrate(context.Background(), db.Table(&ListInItem{})); err != nil {
		t.Fatal(err)
	}
	return New[ListInItem](gdb, log.Empty(),
		append([]StoreOption{WithQueryFields("id", "code", "grade")}, opts...)...)
}

// seedListInItems creates n rows keyed k0000..k(n-1) and returns the keys.
func seedListInItems(t *testing.T, s *Store[ListInItem], n int, grade string) []string {
	t.Helper()
	objs := make([]*ListInItem, 0, n)
	keys := make([]string, 0, n)
	for i := range n {
		k := fmt.Sprintf("k%04d", i)
		keys = append(keys, k)
		objs = append(objs, &ListInItem{Code: k, Grade: grade})
	}
	if err := s.BatchCreate(context.Background(), objs); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestListIn_ChunksPastMaxInList(t *testing.T) {
	s := setupListInStore(t)
	n := where.MaxInList + 5 // 2 chunks: 500 + 5
	keys := seedListInItems(t, s, n, "a")

	// Duplicates land in DIFFERENT chunks than their first occurrence: the
	// set semantics of a single IN must survive chunking — no row twice.
	values := append(append([]string{}, keys...), keys[0], keys[n-1])
	items, err := ListIn(context.Background(), s, "code", values)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("want all %d rows exactly once across chunks, got %d", n, len(items))
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if seen[it.Code] {
			t.Fatalf("row %q returned twice — chunking broke IN's set semantics", it.Code)
		}
		seen[it.Code] = true
	}
}

func TestListIn_RidesTheListReadPath(t *testing.T) {
	// Soft-deleted rows are invisible and extra filter options apply to
	// every chunk — ListIn's semantics are List's, never wider.
	s := setupListInStore(t)
	keys := seedListInItems(t, s, where.MaxInList+2, "a")
	if err := s.Update(context.Background(), Where(where.WithFilter("code", keys[1])), Set(map[string]any{"grade": "b"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), Where(where.WithFilter("code", keys[0]))); err != nil {
		t.Fatal(err)
	}

	items, err := ListIn(context.Background(), s, "code", keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(keys)-1 {
		t.Fatalf("soft-deleted row must stay invisible: want %d, got %d", len(keys)-1, len(items))
	}

	// A filter option narrows every chunk. keys[1] has grade b; the rest a.
	items, err = ListIn(context.Background(), s, "code", keys, where.WithFilter("grade", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Code != keys[1] {
		t.Fatalf("filter option must apply across chunks: got %d rows", len(items))
	}
}

func TestListIn_UnknownFieldRejected(t *testing.T) {
	s := setupListInStore(t)
	_, err := ListIn(context.Background(), s, "typo", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("undeclared field must be rejected with the field named, got %v", err)
	}
}

func TestListIn_FilterOnlyGuard(t *testing.T) {
	s := setupListInStore(t)
	seedListInItems(t, s, 2, "a")
	for name, opt := range map[string]where.Option{
		"order": where.WithOrder("code"),
		"page":  where.WithPage(1, 10),
		"limit": where.WithLimit(10),
		"count": where.WithCount(),
		"cap":   where.WithMaxPageSize(1),
	} {
		if _, err := ListIn(context.Background(), s, "code", []string{"k0000"}, opt); err == nil {
			t.Fatalf("%s option must be rejected — it does not compose across chunks", name)
		}
	}
}

func TestListIn_EmptyValues(t *testing.T) {
	s := setupListInStore(t)
	items, err := ListIn(context.Background(), s, "code", []string(nil))
	if err != nil {
		t.Fatal(err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("empty input must return an empty non-nil slice, got %#v", items)
	}
}

func TestListIn_BypassesStoreMaxPageSize(t *testing.T) {
	// The Store page cap protects client-facing lists; applied per chunk it
	// would silently drop rows mid-set while the result looks complete.
	s := setupListInStore(t, WithMaxPageSize(3))
	keys := seedListInItems(t, s, 10, "a")

	items, err := ListIn(context.Background(), s, "code", keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(keys) {
		t.Fatalf("store max-page-size must not clip chunks: want %d, got %d", len(keys), len(items))
	}
}

// --- ListIn review fixes ------------------------------------------------

// ListInNocase pins database equality that is WIDER than Go equality: a
// case-insensitive column collation makes "a" and "A" the same value to
// the database while Go's map dedup keeps both.
type ListInNocase struct {
	db.Model
	Code string `json:"code" gorm:"type:text COLLATE NOCASE;not null"`
}

func (ListInNocase) RIDPrefix() string { return "lnc" }

// crossChunkCaseValues builds a value set where the lower-case spelling
// lands in chunk one and the upper-case spelling in chunk two, with
// non-matching fillers in between.
func crossChunkCaseValues(lower, upper string) []string {
	values := make([]string, 0, where.MaxInList+2)
	values = append(values, lower)
	for i := range where.MaxInList {
		values = append(values, fmt.Sprintf("filler%04d", i))
	}
	return append(values, upper)
}

func TestListIn_CrossChunkDBEqualityDeduped(t *testing.T) {
	// Review finding #1: dedup ran on Go equality only. Under a
	// case-insensitive collation a row matching "a" in chunk one matched
	// "A" in chunk two as well and was returned twice — a single big IN
	// returns it once. Cross-chunk results dedup by primary key now.
	gdb := setupDB(t)
	if gdb.Unsafe(context.Background()).Dialector.Name() != "sqlite" {
		t.Skip("NOCASE collation spelling is SQLite-specific; the MySQL lane pins its default CI collation")
	}
	if err := gdb.Migrate(context.Background(), db.Table(&ListInNocase{})); err != nil {
		t.Fatal(err)
	}
	s := New[ListInNocase](gdb, log.Empty(), WithQueryFields("id", "code"))
	if err := s.Create(context.Background(), &ListInNocase{Code: "a"}); err != nil {
		t.Fatal(err)
	}

	items, err := ListIn(context.Background(), s, "code", crossChunkCaseValues("a", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("a row matching values in two chunks must be returned once (single-IN set semantics), got %d", len(items))
	}
}

// TestListIn_MySQLCrossChunkCaseInsensitiveDeduped is the same regression
// on real MySQL, whose DEFAULT utf8mb4 collation is case-insensitive — no
// special column spelling required (make test-mysql lane).
func TestListIn_MySQLCrossChunkCaseInsensitiveDeduped(t *testing.T) {
	s := setupListInStoreOn(t, dbtest.OpenMySQL(t))
	if err := s.Create(context.Background(), &ListInItem{Code: "a", Grade: "g"}); err != nil {
		t.Fatal(err)
	}

	items, err := ListIn(context.Background(), s, "code", crossChunkCaseValues("a", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("MySQL CI collation: a row matching values in two chunks must be returned once, got %d", len(items))
	}
}

func TestListIn_EmptyValuesStillValidate(t *testing.T) {
	// Review finding #2: the empty-set fast path skipped every check — a
	// typo'd field, a non-composing option and an unauthenticated
	// fail-closed scope all "succeeded" with a silent empty page. The
	// degenerate pass keeps ListIn(∅) ≡ List(WithFilterIn(field)) over an
	// empty set: validates everything, matches nothing.
	s := setupListInStore(t)

	if _, err := ListIn(context.Background(), s, "typo", []string(nil)); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("empty input must still reject an unknown field, got %v", err)
	}
	if _, err := ListIn(context.Background(), s, "code", []string(nil), where.WithOrder("code")); err == nil {
		t.Fatal("empty input must still trip the filters-only guard")
	}

	owned := setupProductStore(t) // db.OwnedModel → automatic fail-closed OwnerScope
	if _, err := ListIn(context.Background(), owned, "name", []string(nil)); !errors.Is(err, apierr.ErrUnauthenticated) {
		t.Fatalf("empty input must still run fail-closed scopes, got %v", err)
	}
	if _, err := ListIn(userCtx("u1"), owned, "name", []string(nil)); err != nil {
		t.Fatalf("authenticated empty input stays a clean empty page, got %v", err)
	}
}
