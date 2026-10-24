package where

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// The query parameters FromQuery reserves.
var reserved = []string{"page", "size", "order", "cursor"}

// FromQuery turns a list request's query string into options:
//
//	page=2&size=20            Page(2, 20); size alone means page 1
//	order=created_at:desc     Order, also a comma-separated list
//	cursor=<token>            Cursor (empty for the first page)
//	status=draft              Eq; a repeated parameter is In
//
// An empty value filters nothing, as an empty form field means: status=
// is no filter, not a match on the empty string. allowed maps the fields
// the caller may filter and order on to their columns. With strict, a
// parameter that is neither reserved nor allowed is an error; without it,
// such parameters are ignored. Every error wraps ErrInvalidParam or
// ErrUnknownField.
func FromQuery(q url.Values, allowed map[string]string, strict bool) ([]Option, error) {
	var opts []Option
	page, size := 0, 0
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%w: page must be a positive integer, got %q", ErrInvalidParam, v)
		}
		page = n
	}
	if v := q.Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%w: size must be a positive integer, got %q", ErrInvalidParam, v)
		}
		size = n
	}
	if cursor, ok := q["cursor"]; ok {
		c := ""
		if len(cursor) > 0 {
			c = cursor[0]
		}
		if page > 0 {
			return nil, fmt.Errorf("%w: page and cursor cannot be combined", ErrInvalidParam)
		}
		if size == 0 {
			size = -1 // the store's default
		}
		opts = append(opts, cursorOrDefault(c, size))
	} else if page > 0 || size > 0 {
		opts = append(opts, Page(max(page, 1), size))
	}
	if v := q.Get("order"); v != "" {
		for part := range strings.SplitSeq(v, ",") {
			field, dir, _ := strings.Cut(strings.TrimSpace(part), ":")
			if _, ok := allowed[field]; !ok {
				return nil, fmt.Errorf("%w %q: order by one of %s", ErrUnknownField, field, strings.Join(sortedKeys(allowed), ", "))
			}
			switch strings.ToLower(dir) {
			case "", "asc":
				opts = append(opts, Order(field))
			case "desc":
				opts = append(opts, Order(field, true))
			default:
				return nil, fmt.Errorf("%w: order direction must be asc or desc, got %q", ErrInvalidParam, dir)
			}
		}
	}
	for _, key := range sortedKeys(q) {
		if slices.Contains(reserved, key) {
			continue
		}
		if _, ok := allowed[key]; !ok {
			if strict {
				return nil, fmt.Errorf("%w %q: filter on one of %s", ErrUnknownField, key, strings.Join(sortedKeys(allowed), ", "))
			}
			continue
		}
		values := slices.DeleteFunc(slices.Clone(q[key]), func(v string) bool { return v == "" })
		switch len(values) {
		case 0:
		case 1:
			opts = append(opts, Eq(key, values[0]))
		default:
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			opts = append(opts, In(key, args...))
		}
	}
	return opts, nil
}

// cursorOrDefault is Cursor with size -1 meaning the store's default.
func cursorOrDefault(c string, size int) Option {
	if size > 0 {
		return Cursor(c, size)
	}
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasPage, cfg.Cursor, cfg.CursorSize, cfg.FirstPage = true, c, 0, c == ""
		return tx, nil
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
