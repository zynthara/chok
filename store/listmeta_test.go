package store

import (
	"net/url"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// HandleList envelope fix: ListFromQuery returns the pagination the
// query actually executed with (where.PageInfo), same-sourced with the
// SQL LIMIT/OFFSET, so envelope renderers stop re-deriving page/size
// from the raw request and lying when a store cap clamps it.

func TestListFromQuery_PageInfoSameSourceAsSQL(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(t.Context(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](gdb, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"),
		WithMaxPageSize(2))
	alice := userCtx("alice")
	for _, name := range []string{"a", "b", "c"} {
		if err := s.Create(alice, &Product{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	// Oversized request: the meta reports the clamped size, not an echo
	// of the request, and HasMore comes from the true offset.
	items, total, meta, err := s.ListFromQuery(alice, url.Values{"size": {"10"}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 2 {
		t.Fatalf("clamp must hold: total=%d items=%d", total, len(items))
	}
	if want := (where.PageInfo{Page: 1, Size: 2, Offset: 0, HasMore: true}); meta != want {
		t.Fatalf("meta = %+v, want %+v", meta, want)
	}

	// Page 2 counts in clamped units: offset moves with the effective
	// size, and the last page keeps Size = effective LIMIT — never
	// len(items).
	items, _, meta, err = s.ListFromQuery(alice, url.Values{"page": {"2"}, "size": {"10"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("page 2 of size-2 pages over 3 rows: want 1 item, got %d", len(items))
	}
	if want := (where.PageInfo{Page: 2, Size: 2, Offset: 2, HasMore: false}); meta != want {
		t.Fatalf("meta = %+v, want %+v", meta, want)
	}
}

// TestList_PageMetaOnPlainList: the option-driven List carries the same
// meta on Page[T] — HasMore only when the query counted.
func TestList_PageMetaOnPlainList(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(t.Context(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](gdb, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))
	alice := userCtx("alice")
	for _, name := range []string{"a", "b", "c"} {
		if err := s.Create(alice, &Product{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.List(alice, where.WithPage(1, 2), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if want := (where.PageInfo{Page: 1, Size: 2, Offset: 0, HasMore: true}); page.Meta != want {
		t.Fatalf("counted list meta = %+v, want %+v", page.Meta, want)
	}

	// Without WithCount the total is unknown — HasMore stays false and
	// the effective values still report.
	page, err = s.List(alice, where.WithPage(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if want := (where.PageInfo{Page: 2, Size: 2, Offset: 2, HasMore: false}); page.Meta != want {
		t.Fatalf("uncounted list meta = %+v, want %+v", page.Meta, want)
	}
}
