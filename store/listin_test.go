package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #8 regression tests: ListIn is the auto-chunked second half
// of the two-step IN pattern — one IN's set semantics past where.MaxInList,
// under exactly List's allowlist/scope/soft-delete read path.

type ListInItem struct {
	db.SoftDeleteModel
	Key   string `json:"key" gorm:"size:32;not null"`
	Grade string `json:"grade" gorm:"size:8;not null;default:''"`
}

func (ListInItem) RIDPrefix() string { return "lii" }

func setupListInStore(t *testing.T, opts ...StoreOption) *Store[ListInItem] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&ListInItem{})); err != nil {
		t.Fatal(err)
	}
	return New[ListInItem](gdb, log.Empty(),
		append([]StoreOption{WithQueryFields("id", "key", "grade")}, opts...)...)
}

// seedListInItems creates n rows keyed k0000..k(n-1) and returns the keys.
func seedListInItems(t *testing.T, s *Store[ListInItem], n int, grade string) []string {
	t.Helper()
	objs := make([]*ListInItem, 0, n)
	keys := make([]string, 0, n)
	for i := range n {
		k := fmt.Sprintf("k%04d", i)
		keys = append(keys, k)
		objs = append(objs, &ListInItem{Key: k, Grade: grade})
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
	items, err := ListIn(context.Background(), s, "key", values)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("want all %d rows exactly once across chunks, got %d", n, len(items))
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if seen[it.Key] {
			t.Fatalf("row %q returned twice — chunking broke IN's set semantics", it.Key)
		}
		seen[it.Key] = true
	}
}

func TestListIn_RidesTheListReadPath(t *testing.T) {
	// Soft-deleted rows are invisible and extra filter options apply to
	// every chunk — ListIn's semantics are List's, never wider.
	s := setupListInStore(t)
	keys := seedListInItems(t, s, where.MaxInList+2, "a")
	if err := s.Update(context.Background(), Where(where.WithFilter("key", keys[1])), Set(map[string]any{"grade": "b"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), Where(where.WithFilter("key", keys[0]))); err != nil {
		t.Fatal(err)
	}

	items, err := ListIn(context.Background(), s, "key", keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(keys)-1 {
		t.Fatalf("soft-deleted row must stay invisible: want %d, got %d", len(keys)-1, len(items))
	}

	// A filter option narrows every chunk. keys[1] has grade b; the rest a.
	items, err = ListIn(context.Background(), s, "key", keys, where.WithFilter("grade", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != keys[1] {
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
		"order": where.WithOrder("key"),
		"page":  where.WithPage(1, 10),
		"limit": where.WithLimit(10),
		"count": where.WithCount(),
		"cap":   where.WithMaxPageSize(1),
	} {
		if _, err := ListIn(context.Background(), s, "key", []string{"k0000"}, opt); err == nil {
			t.Fatalf("%s option must be rejected — it does not compose across chunks", name)
		}
	}
}

func TestListIn_EmptyValues(t *testing.T) {
	s := setupListInStore(t)
	items, err := ListIn(context.Background(), s, "key", []string(nil))
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

	items, err := ListIn(context.Background(), s, "key", keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(keys) {
		t.Fatalf("store max-page-size must not clip chunks: want %d, got %d", len(keys), len(items))
	}
}
