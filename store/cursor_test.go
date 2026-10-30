package store_test

import (
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// walk pages through s with opts and the cursor size, returning the
// titles in page order and the number of pages.
func walk(t *testing.T, s *store.Store[post], size int, opts ...where.Option) ([]string, int) {
	t.Helper()
	var out []string
	cursor, pages := "", 0
	for {
		page, err := s.ListWithCursor(t.Context(), append(slices.Clone(opts), where.Cursor(cursor, size))...)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		out = append(out, titles(page.Items)...)
		if page.HasMore != (page.Cursor != "") {
			t.Fatalf("HasMore %v with cursor %q", page.HasMore, page.Cursor)
		}
		if page.Cursor == "" {
			return out, pages
		}
		if pages > 100 {
			t.Fatal("no end")
		}
		cursor = page.Cursor
	}
}

func TestListWithCursor_WalksEveryRowOnce(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	for i := range 7 {
		// Views repeat, so the RID breaks the ties.
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("p%d", i), Views: int64(i % 3), Score: float64(i) / 2, Status: []string{"a", "b"}[i%2]})
	}
	all := must(s.List(ctx, where.Order("views"), where.Limit(100))).Items
	for _, c := range []struct {
		name string
		opts []where.Option
	}{
		{"by id", nil},
		{"views asc", []where.Option{where.Order("views")}},
		{"views desc", []where.Option{where.Order("views", true)}},
		{"score", []where.Option{where.Order("score")}},
		{"created_at desc", []where.Option{where.Order("created_at", true)}},
		{"status", []where.Option{where.Order("status")}},
	} {
		for _, size := range []int{1, 2, 3, 7, 50} {
			got, pages := walk(t, s, size, c.opts...)
			if len(got) != len(all) {
				t.Fatalf("%s/%d: %d rows, want %d", c.name, size, len(got), len(all))
			}
			seen := map[string]bool{}
			for _, title := range got {
				if seen[title] {
					t.Fatalf("%s/%d: %s twice in %v", c.name, size, title, got)
				}
				seen[title] = true
			}
			if want := max(1, (len(all)+size-1)/size); pages != want {
				t.Fatalf("%s/%d: %d pages, want %d", c.name, size, pages, want)
			}
		}
	}
	// The order is the order asked for.
	got, _ := walk(t, s, 2, where.Order("score", true))
	if !slices.Equal(got, []string{"p6", "p5", "p4", "p3", "p2", "p1", "p0"}) {
		t.Fatalf("score desc %v", got)
	}
	// A filter narrows every page.
	if got, _ := walk(t, s, 2, where.Eq("status", "a"), where.Order("views")); len(got) != 4 {
		t.Fatalf("filtered %v", got)
	}
}

func TestListWithCursor_PageShape(t *testing.T) {
	s := posts(t, store.Policy(storePolicy(2, 3)))
	ctx := t.Context()
	for i := range 4 {
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("t%d", i)})
	}
	page, err := s.ListWithCursor(ctx)
	if err != nil || page.Size != 2 || len(page.Items) != 2 || page.Cursor == "" || page.Total != 0 {
		t.Fatalf("default size: %+v %v", page, err)
	}
	// A size equal to the max still sees that a next page exists.
	page, _ = s.ListWithCursor(ctx, where.Cursor("", 3))
	if len(page.Items) != 3 || page.Cursor == "" {
		t.Fatalf("size at the cap: %+v", page)
	}
	page, _ = s.ListWithCursor(ctx, where.Cursor("", 10), where.Count())
	if len(page.Items) != 3 || page.Size != 3 || page.Total != 4 {
		t.Fatalf("clamped, counted: %+v", page)
	}
	page, _ = s.ListWithCursor(ctx, where.Cursor("", 3), where.Eq("title", "zzz"))
	if page.Items == nil || page.Cursor != "" || page.HasMore {
		t.Fatalf("empty: %+v", page)
	}
	// List with a cursor option is a cursor page.
	page, err = s.List(ctx, where.Cursor("", 2))
	if err != nil || page.Cursor == "" || page.Page != 0 {
		t.Fatalf("List with Cursor: %+v %v", page, err)
	}
	for name, opts := range map[string][]where.Option{
		"page":      {where.Page(1, 2), where.Cursor("", 2)},
		"limit":     {where.Limit(2), where.Cursor("", 2)},
		"offset":    {where.Offset(1), where.Cursor("", 2)},
		"two order": {where.Order("title"), where.Order("views"), where.Cursor("", 2)},
		"size 0":    {where.Cursor("", 0)},
		"size big":  {where.Cursor("", 10_001)},
		"nullable":  {where.Order("published_at"), where.Cursor("", 2)},
	} {
		if _, err := s.ListWithCursor(ctx, opts...); !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestListWithCursor_RejectsForeignCursors(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	for i := range 3 {
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("t%d", i), Views: int64(i)})
	}
	page := must(s.ListWithCursor(ctx, where.Order("views"), where.Cursor("", 1)))
	token := page.Cursor
	for name, opts := range map[string][]where.Option{
		"other field":     {where.Order("title"), where.Cursor(token, 1)},
		"other direction": {where.Order("views", true), where.Cursor(token, 1)},
		"not base64":      {where.Order("views"), where.Cursor("!!!", 1)},
		"not json":        {where.Order("views"), where.Cursor(base64.RawURLEncoding.EncodeToString([]byte("nope")), 1)},
		"too long":        {where.Order("views"), where.Cursor(strings.Repeat("A", 5000), 1)},
		"forged kind":     {where.Order("views"), where.Cursor(forge(`{"v":1,"f":"views","d":false,"k":"str","x":"1","r":"pst_x"}`), 1)},
		"forged value":    {where.Order("views"), where.Cursor(forge(`{"v":1,"f":"views","d":false,"k":"int","x":"one","r":"pst_x"}`), 1)},
		"no tie-break":    {where.Order("views"), where.Cursor(forge(`{"v":1,"f":"views","d":false,"k":"int","x":"1","r":""}`), 1)},
		"old version":     {where.Order("views"), where.Cursor(forge(`{"v":0,"f":"views","d":false,"k":"int","x":"1","r":"pst_x"}`), 1)},
	} {
		if _, err := s.ListWithCursor(ctx, opts...); !errors.Is(err, chok.ErrBadRequest) || !errors.Is(err, where.ErrInvalidParam) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The token is readable JSON carrying the public RID, never the
	// numeric key.
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	if !strings.Contains(string(raw), `"r":"pst_`) || strings.Contains(string(raw), `"id"`) {
		t.Fatalf("token %s", raw)
	}
	// The same token under the same order continues.
	next, err := s.ListWithCursor(ctx, where.Order("views"), where.Cursor(token, 1))
	if err != nil || titles(next.Items)[0] != "t1" {
		t.Fatalf("continue: %+v %v", next, err)
	}
	// A forged but well-formed boundary is just a position.
	if page, err := s.ListWithCursor(ctx, where.Order("views"), where.Cursor(forge(`{"v":1,"f":"views","d":false,"k":"int","x":"1","r":"zzzz"}`), 5)); err != nil || len(page.Items) != 1 {
		t.Fatalf("forged position: %+v %v", page, err)
	}
}

func forge(js string) string { return base64.RawURLEncoding.EncodeToString([]byte(js)) }

// narrow keys a cursor by an int8 column and a defined string type.
type narrow struct {
	db.Model
	Small int8      `json:"small" store:"query" gorm:"not null"`
	Kind  patchKind `json:"kind" store:"query" gorm:"size:20;not null"`
	Note  *string   `json:"note" store:"query" gorm:"size:20"`
}

func TestListWithCursor_FieldTypes(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&narrow{})))
	s := store.New[narrow](d)
	ctx := t.Context()
	for i := range 5 {
		mustCreate(t, ctx, s, &narrow{Small: int8(i), Kind: patchKind(fmt.Sprintf("k%d", 4-i))})
	}
	for _, field := range []string{"small", "kind", "created_at", "id"} {
		count, cursor := 0, ""
		for range 10 {
			page, err := s.ListWithCursor(ctx, where.Order(field), where.Cursor(cursor, 2))
			if err != nil {
				t.Fatalf("%s: %v", field, err)
			}
			count += len(page.Items)
			if cursor = page.Cursor; cursor == "" {
				break
			}
		}
		if count != 5 {
			t.Fatalf("%s: walked %d rows", field, count)
		}
	}
	// A value out of the column's range is refused before the query.
	overflow := forge(`{"v":1,"f":"small","d":false,"k":"int","x":"300","r":"x"}`)
	if _, err := s.ListWithCursor(ctx, where.Order("small"), where.Cursor(overflow, 2)); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("int8 overflow: %v", err)
	}
	if _, err := s.ListWithCursor(ctx, where.Order("note"), where.Cursor("", 2)); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("a nullable field cannot key a cursor: %v", err)
	}
}

// TestListWithCursor_TimesAcrossZones pins the time boundary: rows
// stamped in UTC page correctly whatever zone the cursor was made in.
func TestListWithCursor_TimesAcrossZones(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	base := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC) // a DST change in New York
	for i := range 6 {
		p := &post{Title: fmt.Sprintf("t%d", i)}
		mustCreate(t, ctx, s, p)
		at := base.Add(time.Duration(i) * 20 * time.Minute)
		if err := s.Unsafe(ctx).Where("id = ?", p.ID).UpdateColumn("created_at", at).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, _ := walk(t, s, 2, where.Order("created_at"))
	if !slices.Equal(got, []string{"t0", "t1", "t2", "t3", "t4", "t5"}) {
		t.Fatalf("created_at asc %v", got)
	}
	got, _ = walk(t, s, 4, where.Order("created_at", true))
	if !slices.Equal(got, []string{"t5", "t4", "t3", "t2", "t1", "t0"}) {
		t.Fatalf("created_at desc %v", got)
	}
}

func TestListFromQuery_Cursor(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	for i := range 3 {
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("t%d", i)})
	}
	page, err := s.ListFromQuery(ctx, map[string][]string{"cursor": {""}, "size": {"2"}, "order": {"title:desc"}})
	if err != nil || page.Total != 3 || len(page.Items) != 2 || page.Cursor == "" || titles(page.Items)[0] != "t2" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = s.ListFromQuery(ctx, map[string][]string{"cursor": {page.Cursor}, "size": {"2"}, "order": {"title:desc"}})
	if err != nil || len(page.Items) != 1 || page.Cursor != "" || titles(page.Items)[0] != "t0" {
		t.Fatalf("last page: %+v %v", page, err)
	}
}

// clock is modelled on gorm.io/datatypes.Time: an integer Go type whose
// wire value is a string. The cursor follows the wire.
type clock time.Duration

func (c clock) Value() (driver.Value, error) { return time.Duration(c).String(), nil }

func (c *clock) Scan(v any) error {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Errorf("clock: cannot scan %T", v)
	}
	d, err := time.ParseDuration(s)
	*c = clock(d)
	return err
}

// keyed orders cursors by a Valuer, a serializer and a long string.
// No column may be a MySQL reserved word (long is one): where does not
// quote identifiers.
type keyed struct {
	db.Model
	Grp   string `json:"grp" store:"query" gorm:"size:20;not null;default:''"`
	Clock clock  `json:"clock" store:"query" gorm:"size:20;not null"`
	Ts    int64  `json:"ts" store:"query" gorm:"serializer:unixtime;type:timestamp;not null"`
	Line  string `json:"line" store:"query" gorm:"size:1100;not null;default:''"`
	Blob  []byte `json:"blob" store:"query"`
}

func keyedStore(t *testing.T) *store.Store[keyed] {
	t.Helper()
	return store.New[keyed](dbtest.Open(t, db.Tables(db.Table(&keyed{}))))
}

func walkKeyed(t *testing.T, s *store.Store[keyed], field string, size int) (int, error) {
	t.Helper()
	seen, cursor := map[string]bool{}, ""
	for range 50 {
		page, err := s.ListWithCursor(t.Context(), where.Order(field), where.Cursor(cursor, size))
		if err != nil {
			return len(seen), err
		}
		for _, it := range page.Items {
			if seen[it.RID] {
				t.Fatalf("%s: %s twice", field, it.RID)
			}
			seen[it.RID] = true
		}
		if cursor = page.Cursor; cursor == "" {
			return len(seen), nil
		}
	}
	t.Fatalf("%s: no end", field)
	return 0, nil
}

// TestListWithCursor_TiesOnTheEmptyString: an empty string is a boundary
// like any other, not a first-page marker, and ties break on the RID.
func TestListWithCursor_TiesOnTheEmptyString(t *testing.T) {
	s := keyedStore(t)
	ctx := t.Context()
	for i, grp := range []string{"", "", "", "a", "a"} {
		mustCreate(t, ctx, s, &keyed{Grp: grp, Clock: clock(i), Ts: int64(1_000 + i)})
	}
	for size := 1; size <= 5; size++ {
		if n, err := walkKeyed(t, s, "grp", size); err != nil || n != 5 {
			t.Fatalf("size %d: %d rows, %v", size, n, err)
		}
	}
}

// TestListWithCursor_ValuerAndSerializerFields: the kind of a boundary is
// what the column receives — a string from the clock's Valuer, a time
// from the unixtime serializer — so the tokens a page issues are the
// ones the next page accepts.
func TestListWithCursor_ValuerAndSerializerFields(t *testing.T) {
	s := keyedStore(t)
	ctx := t.Context()
	for i := range 5 {
		mustCreate(t, ctx, s, &keyed{Clock: clock(time.Duration(i) * time.Second), Ts: int64(1_000 + 10*i)})
	}
	for _, field := range []string{"clock", "ts"} {
		if n, err := walkKeyed(t, s, field, 2); err != nil || n != 5 {
			t.Fatalf("%s: %d rows, %v", field, n, err)
		}
	}
	page := must(s.ListWithCursor(ctx, where.Order("ts"), where.Cursor("", 2)))
	raw, _ := base64.RawURLEncoding.DecodeString(page.Cursor)
	if !strings.Contains(string(raw), `"k":"time"`) {
		t.Fatalf("a serializer field keys by its wire kind: %s", raw)
	}
	// A column without a scalar kind cannot key a page; that is decided
	// before any row is read.
	if _, err := s.ListWithCursor(ctx, where.Order("blob"), where.Cursor("", 2)); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("a bytes field: %v", err)
	}
}

// TestListWithCursor_LongStringBoundaries: a boundary at the 1,024-byte
// limit signs and is consumed; a longer one with a next page fails as a
// server error rather than issuing a token the next request refuses.
func TestListWithCursor_LongStringBoundaries(t *testing.T) {
	s := keyedStore(t)
	ctx := t.Context()
	for _, p := range []string{"a", "b", "c"} {
		mustCreate(t, ctx, s, &keyed{Line: p + strings.Repeat("x", 1023), Ts: 1})
	}
	if n, err := walkKeyed(t, s, "line", 1); err != nil || n != 3 {
		t.Fatalf("1,024-byte boundaries: %d rows, %v", n, err)
	}
	mustCreate(t, ctx, s, &keyed{Line: "0" + strings.Repeat("y", 1024), Ts: 1})
	_, err := s.ListWithCursor(ctx, where.Order("line"), where.Cursor("", 1))
	var coder chok.Coder
	if err == nil || errors.As(err, &coder) || !strings.Contains(err.Error(), "over 1024") {
		t.Fatalf("a 1,025-byte boundary: %v", err)
	}
	// On the last page no token is needed, so nothing fails.
	if page, err := s.ListWithCursor(ctx, where.Order("line", true), where.Eq("line", "0"+strings.Repeat("y", 1024)), where.Cursor("", 1)); err != nil || page.Cursor != "" {
		t.Fatalf("a long boundary on the last page: %+v %v", page, err)
	}
}

// TestListWithCursor_CustomOptionRunsOnce: an option runs once per page,
// on the one scoped query.
func TestListWithCursor_CustomOptionRunsOnce(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &post{Title: "a"}, &post{Title: "b"})
	calls := 0
	counting := where.Option(func(tx *gorm.DB, _ *where.Config, _ map[string]string) (*gorm.DB, error) {
		calls++
		return tx, nil
	})
	if _, err := s.ListWithCursor(ctx, counting, where.Cursor("", 1)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the option ran %d times", calls)
	}
}
