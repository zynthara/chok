package where_test

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/store/where"
)

func parse(t *testing.T, raw string, strict bool) ([]where.Option, error) {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return where.FromQuery(q, fields, strict)
}

func TestFromQuery_Pagination(t *testing.T) {
	gdb := seed(t, "a")
	for _, c := range []struct {
		query               string
		page, limit, offset int
		cursor              string
		cursorSize          int
		first, paged        bool
	}{
		{query: "", paged: false},
		{query: "page=3&size=5", page: 3, limit: 5, offset: 10, paged: true},
		{query: "size=5", page: 1, limit: 5, paged: true},
		{query: "page=2", page: 2, limit: 0, paged: true}, // the store's default size
		{query: "cursor=", first: true, paged: true},
		{query: "cursor=tok&size=3", cursor: "tok", cursorSize: 3, paged: true},
		{query: "cursor=tok", cursor: "tok", cursorSize: 0, paged: true},
	} {
		opts, err := parse(t, c.query, true)
		if err != nil {
			t.Fatalf("%q: %v", c.query, err)
		}
		_, cfg, err := apply(t, gdb, opts...)
		if err != nil {
			t.Fatalf("%q: apply: %v", c.query, err)
		}
		if cfg.HasPage != c.paged || cfg.PageNumber != c.page || cfg.Limit != c.limit || cfg.Offset != c.offset ||
			cfg.Cursor != c.cursor || cfg.CursorSize != c.cursorSize || cfg.FirstPage != c.first || cfg.Count {
			t.Errorf("%q: %+v", c.query, cfg)
		}
	}
}

func TestFromQuery_FiltersAndOrder(t *testing.T) {
	gdb := seed(t, "a", "b", "c")
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"name=b", []string{"b"}},
		{"name=a&name=c", []string{"a", "c"}}, // a repeated parameter is IN
		{"name=a&n=0", []string{"a"}},
		{"name=", []string{"a", "b", "c"}}, // an empty value is no filter
		{"name=&name=c", []string{"c"}},
		{"order=name:desc,n", []string{"a", "b", "c"}}, // order is recorded, not a filter
	} {
		opts, err := parse(t, c.query, true)
		if err != nil {
			t.Fatalf("%q: %v", c.query, err)
		}
		match(t, gdb, c.want, opts...)
	}
	opts, _ := parse(t, "order=name:DESC,n:asc,id", true)
	_, cfg, _ := apply(t, gdb, opts...)
	var got []string
	for _, o := range cfg.Orders {
		got = append(got, o.Field+map[bool]string{true: " desc", false: " asc"}[o.Desc])
	}
	if !slices.Equal(got, []string{"name desc", "n asc", "id asc"}) {
		t.Fatalf("orders %v", got)
	}
}

func TestFromQuery_Refusals(t *testing.T) {
	for _, c := range []struct {
		query string
		want  error
	}{
		{"page=abc", where.ErrInvalidParam},
		{"page=0", where.ErrInvalidParam},
		{"page=-2", where.ErrInvalidParam},
		{"size=x", where.ErrInvalidParam},
		{"size=0", where.ErrInvalidParam},
		{"page=1&cursor=", where.ErrInvalidParam},
		{"order=nope", where.ErrUnknownField},
		{"order=name:sideways", where.ErrInvalidParam},
		{"nope=1", where.ErrUnknownField},
		{"nope=", where.ErrUnknownField}, // the name is checked even without a value
	} {
		if _, err := parse(t, c.query, true); !errors.Is(err, c.want) {
			t.Errorf("%q: %v, want %v", c.query, err, c.want)
		}
	}
	// The unknown-parameter error lists what may be filtered on.
	_, err := parse(t, "bogus=1", true)
	if err == nil || !strings.Contains(err.Error(), "filter on one of id, n, name, note") {
		t.Fatalf("message: %v", err)
	}
	// An oversized page passes the parse and fails as the option runs.
	opts, err := parse(t, "size=20000", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := apply(t, seed(t), opts...); !errors.Is(err, where.ErrInvalidParam) {
		t.Fatalf("size 20000: %v", err)
	}
}

func TestFromQuery_Lenient(t *testing.T) {
	gdb := seed(t, "a", "b")
	opts, err := parse(t, "name=a&bogus=1&page=1", false)
	if err != nil {
		t.Fatalf("unknown parameters are ignored without strict: %v", err)
	}
	match(t, gdb, []string{"a"}, opts...)
	// An order field still has to exist: it would reach the SQL.
	if _, err := parse(t, "order=bogus", false); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("lenient order: %v", err)
	}
	// Without a field map only pagination is understood.
	q, _ := url.ParseQuery("page=2&size=10&name=a")
	opts, err = where.FromQuery(q, nil, false)
	if err != nil || len(opts) != 1 {
		t.Fatalf("nil map, lenient: %d options, %v", len(opts), err)
	}
	if _, err := where.FromQuery(q, nil, true); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("nil map, strict: %v", err)
	}
}
